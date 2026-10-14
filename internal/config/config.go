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

	// Web UI: the station whose public stream the player tunes into.
	StationName       string
	StreamURL         string
	StreamURLFallback string // "" disables the fallback
	HeroImageURL      string // "" means a plain gradient background
}

// Defaults for the web UI. The demo plays Faith FM Australia's public stream, as the RadioApp
// pilot does; the browser connects to it directly (its Icecast refuses cloud egress IPs).
const (
	DefaultStationName       = "Faith FM Australia"
	DefaultStreamURL         = "https://stream1.faithfm.com.au/FaithFM.mp3"
	DefaultStreamURLFallback = "https://stream1.faithfm.com.au/FaithFM.ogg"
	DefaultHeroImageURL      = "https://cdn.faithfm.com.au/wp-content/uploads/2021/07/23221023/Faith-FM-Hero-Play.jpg"
)

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

		StationName:       l.str("STATION_NAME", DefaultStationName),
		StreamURL:         l.webURL("STREAM_URL", DefaultStreamURL),
		StreamURLFallback: l.webURL("STREAM_URL_FALLBACK", DefaultStreamURLFallback),
		HeroImageURL:      l.webURL("HERO_IMAGE_URL", DefaultHeroImageURL),
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

// webURL validates an optional http(s) URL loaded by the browser. "none" switches the feature off
// explicitly, because an empty variable means "use the default".
func (l *loader) webURL(key, def string) string {
	v := l.str(key, def)
	if v == "none" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		l.fail(key, "must be an http(s) URL or \"none\", got %q", v)
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
