package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// RequestIDHeader carries the correlation id across the proxy, the legacy API and the logs.
const RequestIDHeader = "X-Request-ID"

type requestIDKey struct{}

// RequestID reuses the caller's X-Request-ID or generates one, exposes it in the context and
// echoes it in the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
			// Written back into the inbound headers on purpose: the reverse proxy copies them, so
			// the legacy API receives the same id and both sides can be correlated in the logs.
			r.Header.Set(RequestIDHeader, id)
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// RequestIDFrom returns the id stored by RequestID, or "" outside of it.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b)
}

// validRequestID only accepts short, plain tokens: the id is client-controlled and ends up in
// logs and upstream headers, so anything else (newlines, huge values) is replaced.
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// StatusWriter records the status code written by the wrapped handler.
type StatusWriter struct {
	http.ResponseWriter
	Status      int
	wroteHeader bool
}

func (w *StatusWriter) WriteHeader(code int) {
	// 1xx responses are informational; the final status comes later.
	if !w.wroteHeader && code >= 200 {
		w.Status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *StatusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer (deadlines, hijacking).
func (w *StatusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Flush keeps streaming responses streaming when they pass through the wrapper.
func (w *StatusWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ServedByHeader is set by whoever answers the request (legacy or go) and surfaces in the access log.
const ServedByHeader = "X-Served-By"

// AccessLog emits one structured line per request.
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &StatusWriter{ResponseWriter: w}

			// Deferred so the line is written even when the handler aborts with a panic.
			defer func() {
				status := sw.Status
				if status == 0 {
					status = http.StatusOK
				}
				level := slog.LevelInfo
				// Probes hit these every few seconds; at info they would drown the real traffic.
				if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
					level = slog.LevelDebug
				}
				log.LogAttrs(r.Context(), level, "request",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", status),
					slog.Int64("duration_ms", time.Since(start).Milliseconds()),
					slog.String("served_by", sw.Header().Get(ServedByHeader)),
					slog.String("request_id", RequestIDFrom(r.Context())),
				)
			}()

			next.ServeHTTP(sw, r)
		})
	}
}

// Recover turns a panic into a 500 JSON response instead of a dropped connection.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				// ErrAbortHandler is net/http's (and ReverseProxy's) way to abort a response
				// mid-stream, e.g. when the client disconnects. It must reach the server untouched.
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("panic recovered",
					"panic", v,
					"path", r.URL.Path,
					"request_id", RequestIDFrom(r.Context()),
					"stack", string(debug.Stack()),
				)
				// Best effort: if the handler already started the body, this cannot fix the status.
				Error(w, http.StatusInternalServerError, "internal error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}
