// Package store owns the Postgres connection pool and the database schema.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationLockKey is an arbitrary constant shared by every instance of the binary. Holding it
// serializes migrations when several replicas boot at the same time (rolling deploys, scale-out).
const migrationLockKey int64 = 7_349_120_001

// Store wraps the connection pool. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open creates the pool and verifies connectivity once.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// A small pool on purpose: the proxy's database work (rules, shadow results) is light, and the
	// Supabase session pooler holds one server connection per client connection, so a large pool
	// would burn the project's connection limit for nothing.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	// Recycling connections stops us from holding dead sockets after a pooler restart or failover.
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases every connection in the pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping runs a real query rather than a protocol-level ping: it proves the pooler routes queries
// and the schema is in place, which is what readiness actually means. The keepalive uses it too,
// so an idle free-tier database sees genuine activity.
func (s *Store) Ping(ctx context.Context) error {
	var n int
	return s.db.QueryRowContext(ctx, `SELECT count(*) FROM route_rules`).Scan(&n)
}

// Migrate applies pending embedded migrations in filename order, each in its own transaction.
func (s *Store) Migrate(ctx context.Context, log *slog.Logger) error {
	// Advisory locks belong to the session, so lock, migrate and unlock must share one connection.
	// This is also why DATABASE_URL must point at the session pooler: the transaction pooler can
	// hand each statement a different server connection and the lock would be meaningless.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer func() {
		// Unlock even if ctx was cancelled mid-migration, otherwise other replicas would wait on us.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, uerr := conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockKey); uerr != nil {
			log.Warn("advisory unlock failed, discarding connection", "err", uerr)
			// Returning the connection to the pool would leave the lock held by an idle session;
			// ErrBadConn makes database/sql close it, and closing the session releases the lock.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	// Zero-padded prefixes (001_, 002_) make lexical order the intended order.
	sort.Strings(files)

	for _, file := range files {
		version := strings.TrimSuffix(path.Base(file), ".sql")
		if applied[version] {
			log.Debug("migration already applied", "version", version)
			continue
		}
		body, err := migrationsFS.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}
		start := time.Now()
		if err := applyMigration(ctx, conn, version, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", version, err)
		}
		log.Info("migration applied", "version", version, "duration_ms", time.Since(start).Milliseconds())
	}
	return nil
}

func appliedVersions(ctx context.Context, conn *sql.Conn) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// applyMigration runs the file and records its version atomically, so a failure halfway leaves
// neither partial schema nor a version row claiming it was applied.
func applyMigration(ctx context.Context, conn *sql.Conn, version, body string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit

	// Without arguments lib/pq uses the simple query protocol, which accepts a multi-statement file.
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	return tx.Commit()
}
