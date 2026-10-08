package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
}

func TestChainFirstIsOutermost(t *testing.T) {
	var order []string
	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "handler") }),
		mark("a"), mark("b"), mark("c"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := strings.Join(order, ","); got != "a,b,c,handler" {
		t.Errorf("order = %s", got)
	}
}

func TestRequestID(t *testing.T) {
	hex16 := regexp.MustCompile(`^[0-9a-f]{16}$`)
	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{"generated when missing", "", false},
		{"propagated when valid", "abc-123_X.y", true},
		{"replaced when it could inject log lines", "evil\nlevel=ERROR", false},
		{"replaced when too long", strings.Repeat("a", 65), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var inHeader, inCtx string
			h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				inHeader = r.Header.Get(RequestIDHeader)
				inCtx = RequestIDFrom(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.incoming != "" {
				req.Header.Set(RequestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			out := rec.Header().Get(RequestIDHeader)
			if tt.keep && out != tt.incoming {
				t.Errorf("id = %q, want %q", out, tt.incoming)
			}
			if !tt.keep && !hex16.MatchString(out) {
				t.Errorf("generated id %q is not 16 hex chars", out)
			}
			if inHeader != out || inCtx != out {
				t.Errorf("header %q / context %q / response %q differ", inHeader, inCtx, out)
			}
		})
	}
}

func TestStatusWriterSupportsResponseController(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &StatusWriter{ResponseWriter: rec}
	if err := http.NewResponseController(sw).Flush(); err != nil {
		t.Fatalf("flush through wrapper: %v", err)
	}
	if !rec.Flushed || sw.Status != http.StatusOK {
		t.Errorf("flushed=%v status=%d", rec.Flushed, sw.Status)
	}
}

func TestAccessLogLine(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(ServedByHeader, "legacy")
		w.WriteHeader(http.StatusTeapot)
	}), RequestID, AccessLog(log))

	req := httptest.NewRequest(http.MethodGet, "/api/tracks", nil)
	req.Header.Set(RequestIDHeader, "req-1")
	h.ServeHTTP(httptest.NewRecorder(), req)

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not a single JSON line: %q", buf.String())
	}
	want := map[string]any{"method": "GET", "path": "/api/tracks", "status": float64(418), "served_by": "legacy", "request_id": "req-1"}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	if _, ok := line["duration_ms"]; !ok {
		t.Error("missing duration_ms")
	}
}

func TestAccessLogProbesAtDebug(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := AccessLog(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if buf.Len() != 0 {
		t.Errorf("healthz logged at info: %s", buf.String())
	}
}

func TestRecoverReturnsJSON500(t *testing.T) {
	h := Recover(discardLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"error":"internal error"}` {
		t.Errorf("body = %s", body)
	}
}

func TestRecoverRepanicsAbortHandler(t *testing.T) {
	h := Recover(discardLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}
