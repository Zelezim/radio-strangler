package proxy

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Zelezim/radio-strangler/internal/httpx"
	"github.com/Zelezim/radio-strangler/internal/routing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
}

func TestProxyForwardsToLegacy(t *testing.T) {
	var got *http.Request
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer legacy.Close()
	target, _ := url.Parse(legacy.URL)

	table := routing.NewTable()
	table.Replace([]routing.Rule{
		{Route: "/api/tracks", Mode: routing.ModeShadow},
		{Route: "/api/programs", Mode: routing.ModeGo},
	})
	log := quietLogger()
	h := httpx.Chain(New(table, NewLegacy(target, 5*time.Second, log), log), httpx.RequestID)

	tests := []struct {
		path     string
		wantMode string
	}{
		{"/api/tracks?limit=2", "shadow"},
		{"/api/programs", "go"},        // no Go API yet: still served by legacy
		{"/api/now-playing", "legacy"}, // no rule: default
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com"+tt.path, nil)
			req.RemoteAddr = "203.0.113.7:5555"
			req.Header.Set("X-Forwarded-For", "198.51.100.1")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK || rec.Body.String() != `{"data":[]}` {
				t.Fatalf("got %d %q", rec.Code, rec.Body.String())
			}
			if v := rec.Header().Get(httpx.ServedByHeader); v != "legacy" {
				t.Errorf("X-Served-By = %q", v)
			}
			if v := rec.Header().Get(RouteModeHeader); v != tt.wantMode {
				t.Errorf("X-Route-Mode = %q, want %q", v, tt.wantMode)
			}
			if got.Host != target.Host {
				t.Errorf("upstream Host = %q, want %q", got.Host, target.Host)
			}
			if got.URL.RequestURI() != tt.path {
				t.Errorf("upstream URI = %q, want %q", got.URL.RequestURI(), tt.path)
			}
			if xff := got.Header.Get("X-Forwarded-For"); xff != "198.51.100.1, 203.0.113.7" {
				t.Errorf("X-Forwarded-For = %q", xff)
			}
			if id := got.Header.Get(httpx.RequestIDHeader); id == "" || id != rec.Header().Get(httpx.RequestIDHeader) {
				t.Errorf("request id not propagated: upstream %q, response %q", id, rec.Header().Get(httpx.RequestIDHeader))
			}
		})
	}
}

func TestLegacyDownReturns502(t *testing.T) {
	legacy := httptest.NewServer(http.NotFoundHandler())
	target, _ := url.Parse(legacy.URL)
	legacy.Close() // nothing listens on target any more

	log := quietLogger()
	h := New(routing.NewTable(), NewLegacy(target, time.Second, log), log)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tracks", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"error":"legacy backend unavailable"}` {
		t.Errorf("body = %s", body)
	}
	if v := rec.Header().Get(httpx.ServedByHeader); v != "legacy" {
		t.Errorf("X-Served-By = %q", v)
	}
}

func TestClientKey(t *testing.T) {
	tests := []struct {
		name   string
		header map[string]string
		remote string
		want   string
	}{
		{"explicit client id wins", map[string]string{"X-Client-ID": "app-42", "X-Forwarded-For": "1.1.1.1"}, "9.9.9.9:1", "app-42"},
		{"first forwarded hop", map[string]string{"X-Forwarded-For": " 1.1.1.1 , 2.2.2.2"}, "9.9.9.9:1", "1.1.1.1"},
		{"remote addr without port", nil, "9.9.9.9:1234", "9.9.9.9"},
		{"ipv6 remote addr", nil, "[2001:db8::1]:443", "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remote
			for k, v := range tt.header {
				req.Header.Set(k, v)
			}
			if got := ClientKey(req); got != tt.want {
				t.Errorf("ClientKey = %q, want %q", got, tt.want)
			}
		})
	}
}
