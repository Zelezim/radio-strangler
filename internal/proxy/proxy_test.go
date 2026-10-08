package proxy

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
	candidate := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("candidate must not be called for legacy-served routes")
	})
	h := httpx.Chain(New(table, NewLegacy(target, 5*time.Second, log), candidate, nil, nil, log), httpx.RequestID)

	tests := []struct {
		path     string
		wantMode string
	}{
		{"/api/tracks?limit=2", "shadow"},
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

func TestProxyServesGoRoutesFromCandidate(t *testing.T) {
	legacy := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("legacy must not be called for go routes")
	})
	candidate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"from": "go"})
	})
	table := routing.NewTable()
	table.Replace([]routing.Rule{{Route: "/api/programs", Mode: routing.ModeGo}})

	rec := httptest.NewRecorder()
	New(table, legacy, candidate, nil, nil, quietLogger()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/programs", nil))

	if body := strings.TrimSpace(rec.Body.String()); body != `{"from":"go"}` {
		t.Errorf("body = %s", body)
	}
	if v := rec.Header().Get(httpx.ServedByHeader); v != "go" {
		t.Errorf("X-Served-By = %q, want go", v)
	}
	if v := rec.Header().Get(RouteModeHeader); v != "go" {
		t.Errorf("X-Route-Mode = %q, want go", v)
	}
}

func TestCanarySplitsByClient(t *testing.T) {
	served := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(name)) })
	}
	table := routing.NewTable()
	table.Replace([]routing.Rule{{Route: "/api/tracks", Mode: routing.ModeCanary, CanaryPercent: 50}})
	p := New(table, served("legacy"), served("go"), nil, nil, quietLogger())

	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/tracks", nil)
		req.Header.Set(ClientIDHeader, "client-"+strconv.Itoa(i))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Body.String() != rec.Header().Get(httpx.ServedByHeader) {
			t.Fatalf("X-Served-By %q but body from %q", rec.Header().Get(httpx.ServedByHeader), rec.Body.String())
		}
		counts[rec.Body.String()]++
	}
	if counts["go"] == 0 || counts["legacy"] == 0 {
		t.Errorf("canary did not split traffic: %v", counts)
	}
}

type recordingObserver struct{ got []string }

func (o *recordingObserver) ObserveRequest(route, backend string, status int, _ time.Duration) {
	o.got = append(o.got, route+" "+backend+" "+strconv.Itoa(status))
}

func TestObserverUsesRuleRouteNotRawPath(t *testing.T) {
	respond := func(status int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
	}
	table := routing.NewTable()
	table.Replace([]routing.Rule{{Route: "/api/tracks", Mode: routing.ModeGo}})
	obs := &recordingObserver{}
	p := New(table, respond(http.StatusNotFound), respond(http.StatusOK), nil, obs, quietLogger())

	for _, path := range []string{"/api/tracks/42", "/api/tracks/43", "/wp-login.php"} {
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	want := "/api/tracks go 200|/api/tracks go 200|unmatched legacy 404"
	if got := strings.Join(obs.got, "|"); got != want {
		t.Errorf("observed %q, want %q", got, want)
	}
}

func TestLegacyDownReturns502(t *testing.T) {
	legacy := httptest.NewServer(http.NotFoundHandler())
	target, _ := url.Parse(legacy.URL)
	legacy.Close() // nothing listens on target any more

	log := quietLogger()
	h := New(routing.NewTable(), NewLegacy(target, time.Second, log), http.NotFoundHandler(), nil, nil, log)
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
