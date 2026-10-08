// Package httpx holds the small HTTP building blocks shared by the proxy and the Go API:
// JSON helpers, middleware composition and the cross-cutting middlewares.
package httpx

import (
	"encoding/json"
	"net/http"
)

// Middleware wraps a handler with extra behaviour.
type Middleware func(http.Handler) http.Handler

// Chain applies mw so that the first middleware is the outermost one, which keeps the call
// site in the same order a request travels through it.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is already sent, so an encoding error can only be a broken connection.
	_ = json.NewEncoder(w).Encode(v)
}

// Error writes the same error envelope the legacy API uses: {"error": msg}.
func Error(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}
