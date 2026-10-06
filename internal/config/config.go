// Package config loads and validates runtime settings from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully validated runtime configuration.
type Config struct {
	Port            string
	DatabaseURL     string
	LegacyURL       string
	AdminToken      string
	LegacyTimeout   time.Duration
	RulesRefresh    time.Duration
	ShadowTimeout   time.Duration
	ShadowRetention time.Duration
	ShadowWorkers   int
	ShadowQueue     int
	DemoInjectBugs  bool
	LogLevel        slog.Level
}

// Load reads the process environment. All problems are reported together so a misconfigured
// deploy is fixed in one round trip instead of one crash per variable.
func Load() (Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (Config, error) {
	l := &loader{getenv: getenv}
	cfg := Config{
		Port:            l.port("PORT", "8080"),
		DatabaseURL:     l.absURL("DATABASE_URL", true),
		LegacyURL:       l.absURL("LEGACY_URL", false),
		AdminToken:      l.str("ADMIN_TOKEN", ""),
		LegacyTimeout:   l.duration("LEGACY_TIMEOUT", 60*time.Second),
		RulesRefresh:    l.duration("RULES_REFRESH", 5*time.Second),
		ShadowTimeout:   l.duration("SHADOW_TIMEOUT", 5*time.Second),
		ShadowRetention: l.duration("SHADOW_RETENTION", 168*time.Hour),
		ShadowWorkers:   l.positiveInt("SHADOW_WORKERS", 4),
		ShadowQueue:     l.positiveInt("SHADOW_QUEUE", 100),
		DemoInjectBugs:  l.boolean("DEMO_INJECT_BUGS", false),
		LogLevel:        l.logLevel("LOG_LEVEL", slog.LevelInfo),
	}
	if err := errors.Join(l.errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loader accumulates validation errors instead of failing on the first one.
type loader struct {
	getenv func(string) string
	errs   []error
}

func (l *loader) fail(key, format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(key+": "+format, args...))
}

func (l *loader) str(key, def string) string {
	if v := strings.TrimSpace(l.getenv(key)); v != "" {
		return v
	}
	return def
}

func (l *loader) port(key, def string) string {
	v := l.str(key, def)
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		l.fail(key, "must be a port number between 1 and 65535, got %q", v)
	}
	return v
}

// absURL validates a required absolute URL. Secret URLs are never echoed back: DATABASE_URL
// carries the password, and config errors end up in logs.
func (l *loader) absURL(key string, secret bool) string {
	v := l.str(key, "")
	if v == "" {
		l.fail(key, "is required")
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		if secret {
			l.fail(key, "must be an absolute URL (scheme://host/...)")
		} else {
			l.fail(key, "must be an absolute URL (scheme://host/...), got %q", v)
		}
		return ""
	}
	return v
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		l.fail(key, "must be a positive duration like 5s or 1m, got %q", v)
		return def
	}
	return d
}

func (l *loader) positiveInt(key string, def int) int {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		l.fail(key, "must be a positive integer, got %q", v)
		return def
	}
	return n
}

func (l *loader) boolean(key string, def bool) bool {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(key, "must be true or false, got %q", v)
		return def
	}
	return b
}

func (l *loader) logLevel(key string, def slog.Level) slog.Level {
	v := l.str(key, "")
	if v == "" {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(v)); err != nil {
		l.fail(key, "must be debug, info, warn or error, got %q", v)
		return def
	}
	return lvl
}
