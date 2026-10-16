package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Zelezim/radio-strangler/internal/httpx"
)

// retryTransport retries idempotent requests that the platform edge answers with 502, 503 or 504.
//
// On Render's free plan the legacy service sleeps after 15 idle minutes. A request from this
// proxy to a sleeping service gets an immediate 502 from the edge ("x-render-routing: no-deploy")
// while it spins up, instead of being held until it is ready. Retrying GET/HEAD for a bounded
// window turns that into a slow first response rather than an error. Writes are never retried:
// a 502 does not prove the backend did not process them.
type retryTransport struct {
	base           http.RoundTripper
	window         time.Duration // total time spent retrying, after which the last answer is returned
	initialBackoff time.Duration
	maxBackoff     time.Duration
	log            *slog.Logger
}

func newRetryTransport(base http.RoundTripper, window time.Duration, log *slog.Logger) *retryTransport {
	return &retryTransport{
		base:           base,
		window:         window,
		initialBackoff: 500 * time.Millisecond,
		maxBackoff:     5 * time.Second,
		log:            log,
	}
}

func retryableStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	idempotent := req.Method == http.MethodGet || req.Method == http.MethodHead
	if !idempotent || (req.Body != nil && req.Body != http.NoBody) {
		return t.base.RoundTrip(req)
	}

	deadline := time.Now().Add(t.window)
	backoff := t.initialBackoff
	for attempt := 1; ; attempt++ {
		resp, err := t.base.RoundTrip(req)
		if err != nil || !retryableStatus(resp.StatusCode) || time.Now().Add(backoff).After(deadline) {
			return resp, err
		}
		// Drain a little so the connection can be reused, then discard this answer.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()

		t.log.Debug("legacy not ready, retrying",
			"status", resp.StatusCode,
			"attempt", attempt,
			"retry_in", backoff.String(),
			"path", req.URL.Path,
			"request_id", httpx.RequestIDFrom(req.Context()),
		)
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, t.maxBackoff)
	}
}
