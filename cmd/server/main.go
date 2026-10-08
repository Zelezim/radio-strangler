// Command server is the Strangler Fig proxy and the new Go station API in one binary.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Zelezim/radio-strangler/internal/config"
	"github.com/Zelezim/radio-strangler/internal/httpx"
	"github.com/Zelezim/radio-strangler/internal/proxy"
	"github.com/Zelezim/radio-strangler/internal/routing"
	"github.com/Zelezim/radio-strangler/internal/store"
)

const (
	// connectTimeout is generous because a cold Supabase project can take a long time to accept
	// the first connection, and under docker compose Postgres may still be starting.
	connectTimeout = 90 * time.Second
	// shutdownTimeout must fit inside the platform's grace period before SIGKILL: Render gives
	// 30s, Docker only 10s by default, so compose needs stop_grace_period of at least 25s.
	shutdownTimeout = 20 * time.Second
)

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
		log.Error("server stopped with error", "err", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(ctx, connectTimeout)
	defer cancelStart()

	st, err := openWithRetry(startCtx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.Migrate(startCtx, log); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.Info("migrations ok")

	table := routing.NewTable()
	loader := routing.NewLoader(table, st, cfg.RulesRefresh, log)
	// Refusing to start without rules is deliberate: serving with an empty table would silently
	// send every shadow/canary/go route back to legacy and hide a broken database.
	if err := loader.Load(startCtx); err != nil {
		return fmt.Errorf("initial rules: %w", err)
	}
	logRules(log, table.Rules())
	go loader.Run(ctx)

	legacyURL, err := url.Parse(cfg.LegacyURL) // already validated by config
	if err != nil {
		return fmt.Errorf("legacy url: %w", err)
	}
	facade := proxy.New(table, proxy.NewLegacy(legacyURL, cfg.LegacyTimeout, log), log)

	mux := http.NewServeMux()
	// Liveness: the process is up. Deliberately independent of the database so a database
	// outage does not make the platform restart a healthy proxy in a loop.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: the process can do real work.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			log.Warn("readiness check failed", "err", err)
			httpx.Error(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.Handle("/", facade)

	srv := &http.Server{
		Addr: ":" + cfg.Port,
		// Recover is innermost so a panic becomes a 500 before AccessLog records the status.
		Handler:           httpx.Chain(mux, httpx.RequestID, httpx.AccessLog(log), httpx.Recover(log)),
		ReadHeaderTimeout: 5 * time.Second, // slowloris protection
		ReadTimeout:       30 * time.Second,
		// Must outlast the legacy timeout, or a slow-but-successful legacy answer would be cut off.
		WriteTimeout: cfg.LegacyTimeout + 15*time.Second,
		IdleTimeout:  120 * time.Second,
		ErrorLog:     slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "legacy_url", cfg.LegacyURL)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}

	// Restore default signal handling: a second Ctrl+C now kills the process immediately.
	stop()
	log.Info("shutting down", "timeout", shutdownTimeout.String())

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}

func logRules(log *slog.Logger, rules []routing.Rule) {
	attrs := make([]any, 0, len(rules))
	for _, r := range rules {
		attrs = append(attrs, slog.String(r.Route, string(r.Mode)))
	}
	log.Info("route rules loaded", slog.Group("rules", attrs...))
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
