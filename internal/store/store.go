// Package store owns the Postgres connection pool and the database schema.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/Zelezim/radio-strangler/internal/radio"
	"github.com/Zelezim/radio-strangler/internal/routing"
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

// ListRules returns every persisted route rule. It satisfies routing.Source.
func (s *Store) ListRules(ctx context.Context) ([]routing.Rule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT route, mode, canary_percent, ignore_fields, updated_at FROM route_rules`)
	if err != nil {
		return nil, fmt.Errorf("query route_rules: %w", err)
	}
	defer rows.Close()

	var rules []routing.Rule
	for rows.Next() {
		var (
			r      routing.Rule
			mode   string
			ignore pq.StringArray
		)
		if err := rows.Scan(&r.Route, &mode, &r.CanaryPercent, &ignore, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan route_rules: %w", err)
		}
		r.Mode = routing.Mode(mode)
		r.IgnoreFields = []string(ignore)
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// ListPrograms returns the schedule with the same SQL formatting and ordering as the legacy API,
// so both implementations are compared on identical data rather than on formatting code.
func (s *Store) ListPrograms(ctx context.Context) ([]radio.Program, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.name, p.host, p.weekday,
		       to_char(p.start_time, 'HH24:MI'),
		       to_char(p.end_time, 'HH24:MI')
		  FROM programs p
		 ORDER BY p.weekday, p.start_time, p.id`)
	if err != nil {
		return nil, fmt.Errorf("query programs: %w", err)
	}
	defer rows.Close()

	var programs []radio.Program
	for rows.Next() {
		var p radio.Program
		if err := rows.Scan(&p.ID, &p.Name, &p.Host, &p.Weekday, &p.StartTime, &p.EndTime); err != nil {
			return nil, fmt.Errorf("scan programs: %w", err)
		}
		programs = append(programs, p)
	}
	return programs, rows.Err()
}

// ListTracks returns the library ordered by id, like the legacy API.
func (s *Store) ListTracks(ctx context.Context) ([]radio.Track, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, title, artist, album, duration_seconds, tags
		  FROM tracks
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query tracks: %w", err)
	}
	defer rows.Close()

	var tracks []radio.Track
	for rows.Next() {
		t, err := scanTrack(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan tracks: %w", err)
		}
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

// NowPlaying returns the most recent play, or nil when nothing has been played yet.
func (s *Store) NowPlaying(ctx context.Context) (*radio.Play, error) {
	var play radio.Play
	t, err := scanTrack(func(dest ...any) error {
		return s.db.QueryRowContext(ctx, `
			SELECT t.id, t.title, t.artist, t.album, t.duration_seconds, t.tags, pl.played_at
			  FROM plays pl
			  JOIN tracks t ON t.id = pl.track_id
			 ORDER BY pl.played_at DESC, pl.id DESC
			 LIMIT 1`).Scan(append(dest, &play.PlayedAt)...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query now playing: %w", err)
	}
	play.Track = t
	return &play, nil
}

// scanTrack reads the common track columns, so the list and now-playing queries cannot drift
// apart in how they map NULL albums and arrays.
func scanTrack(scan func(dest ...any) error) (radio.Track, error) {
	var (
		t     radio.Track
		album sql.NullString
		tags  pq.StringArray
	)
	if err := scan(&t.ID, &t.Title, &t.Artist, &album, &t.DurationSeconds, &tags); err != nil {
		return radio.Track{}, err
	}
	if album.Valid {
		t.Album = &album.String
	}
	t.Tags = []string(tags)
	return t, nil
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
