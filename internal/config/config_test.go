package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"DATABASE_URL": "postgresql://user:pw@db.example.com:5432/postgres?sslmode=require",
		"LEGACY_URL":   "http://localhost:8081",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "8080" || cfg.LegacyTimeout != 60*time.Second || cfg.RulesRefresh != 5*time.Second ||
		cfg.ShadowTimeout != 5*time.Second || cfg.ShadowRetention != 168*time.Hour ||
		cfg.ShadowWorkers != 4 || cfg.ShadowQueue != 100 || cfg.DemoInjectBugs || cfg.LogLevel != slog.LevelInfo {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"PORT":             "9000",
		"DATABASE_URL":     "postgresql://user:pw@db.example.com/postgres",
		"LEGACY_URL":       "http://legacy:8081",
		"ADMIN_TOKEN":      "secret",
		"LEGACY_TIMEOUT":   "30s",
		"SHADOW_WORKERS":   "8",
		"DEMO_INJECT_BUGS": "true",
		"LOG_LEVEL":        "debug",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "9000" || cfg.AdminToken != "secret" || cfg.LegacyTimeout != 30*time.Second ||
		cfg.ShadowWorkers != 8 || !cfg.DemoInjectBugs || cfg.LogLevel != slog.LevelDebug {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadReportsAllErrorsAtOnce(t *testing.T) {
	_, err := load(env(map[string]string{
		"PORT":           "http",
		"LEGACY_URL":     "localhost:8081",
		"SHADOW_TIMEOUT": "-1s",
		"SHADOW_QUEUE":   "0",
		"LOG_LEVEL":      "loud",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"PORT", "DATABASE_URL", "LEGACY_URL", "SHADOW_TIMEOUT", "SHADOW_QUEUE", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}

func TestWebDefaultsAndOverrides(t *testing.T) {
	base := map[string]string{
		"DATABASE_URL": "postgresql://user:pw@db.example.com/postgres",
		"LEGACY_URL":   "http://localhost:8081",
	}
	cfg, err := load(env(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StationName != DefaultStationName || cfg.StreamURL != DefaultStreamURL ||
		cfg.StreamURLFallback != DefaultStreamURLFallback || cfg.HeroImageURL != DefaultHeroImageURL {
		t.Errorf("unexpected web defaults: %+v", cfg)
	}

	base["STATION_NAME"] = "Test FM"
	base["STREAM_URL"] = "https://radio.example/live.mp3"
	base["STREAM_URL_FALLBACK"] = "none"
	base["HERO_IMAGE_URL"] = "none"
	cfg, err = load(env(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StationName != "Test FM" || cfg.StreamURL != "https://radio.example/live.mp3" ||
		cfg.StreamURLFallback != "" || cfg.HeroImageURL != "" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestWebURLsMustBeHTTP(t *testing.T) {
	for _, bad := range []string{"javascript:alert(1)", "ftp://radio.example/x", "/relative.mp3", "https://"} {
		_, err := load(env(map[string]string{
			"DATABASE_URL": "postgresql://user:pw@db.example.com/postgres",
			"LEGACY_URL":   "http://localhost:8081",
			"STREAM_URL":   bad,
		}))
		if err == nil || !strings.Contains(err.Error(), "STREAM_URL") {
			t.Errorf("STREAM_URL=%q: err = %v, want a STREAM_URL error", bad, err)
		}
	}
}

func TestLoadNeverLeaksDatabasePassword(t *testing.T) {
	// A space in the host makes url.Parse fail; its error would normally quote the whole URL.
	_, err := load(env(map[string]string{
		"DATABASE_URL": "postgresql://user:hunter2@bad host/postgres",
		"LEGACY_URL":   "http://localhost:8081",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the password: %v", err)
	}
}
