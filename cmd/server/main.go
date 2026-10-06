// Command server is the Strangler Fig proxy and the new Go station API in one binary.
// For now it only connects to the database and applies migrations.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Zelezim/radio-strangler/internal/config"
	"github.com/Zelezim/radio-strangler/internal/store"
)

// connectTimeout is generous because a cold Supabase project can take a long time to accept the
// first connection, and under docker compose Postgres may still be starting.
const connectTimeout = 90 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	log = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	if err := run(cfg, log); err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	st, err := openWithRetry(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.Migrate(ctx, log); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.Info("migrations ok")
	return nil
}

// openWithRetry keeps trying until ctx expires: a database that is still waking up is a normal
// startup condition, not a reason to crash-loop.
func openWithRetry(ctx context.Context, dsn string, log *slog.Logger) (*store.Store, error) {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		st, err := store.Open(attemptCtx, dsn)
		cancel()
		if err == nil {
			log.Info("database connected", "attempt", attempt)
			return st, nil
		}
		log.Warn("database not ready", "attempt", attempt, "err", err, "retry_in", backoff.String())

		select {
		case <-ctx.Done():
			return nil, errors.Join(fmt.Errorf("connect: giving up after %d attempts", attempt), err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}
