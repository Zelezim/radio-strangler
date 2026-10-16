package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zelezim/radio-strangler/internal/routing"
)

// wakingLegacy answers 502 like Render's edge for the first `sleepy` requests, then 200.
func wakingLegacy(sleepy int32, hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= sleepy {
			w.Header().Set("x-render-routing", "no-deploy")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502</html>"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
}

func fastRetry(window time.Duration) func(*retryTransport) {
	return func(t *retryTransport) {
		t.window = window
		t.initialBackoff = time.Millisecond
		t.maxBackoff = 5 * time.Millisecond
	}
}

func proxyTo(t *testing.T, srv *httptest.Server, tune func(*retryTransport)) http.Handler {
	t.Helper()
	target, _ := url.Parse(srv.URL)
	legacy := NewLegacy(target, 5*time.Second, quietLogger())
	tune(legacy.Transport.(*retryTransport))
	return New(routing.NewTable(), legacy, http.NotFoundHandler(), nil, nil, quietLogger())
}

func TestColdStartIsRetriedForGET(t *testing.T) {
	var hits atomic.Int32
	srv := wakingLegacy(3, &hits)
	defer srv.Close()

	rec := httptest.NewRecorder()
	proxyTo(t, srv, fastRetry(time.Second)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tracks", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != `{"data":[]}` || hits.Load() != 4 {
		t.Errorf("got %d %q after %d attempts, want 200 after 4", rec.Code, rec.Body.String(), hits.Load())
	}
	if rec.Header().Get("x-render-routing") != "" {
		t.Error("headers of a discarded 502 leaked into the final response")
	}
}

func TestWritesAreNeverRetried(t *testing.T) {
	var hits atomic.Int32
	srv := wakingLegacy(1, &hits)
	defer srv.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tracks", strings.NewReader(`{}`))
	proxyTo(t, srv, fastRetry(time.Second)).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway || hits.Load() != 1 {
		t.Errorf("POST: got %d after %d attempts, want the 502 after exactly 1", rec.Code, hits.Load())
	}
}

func TestRetryWindowIsBounded(t *testing.T) {
	var hits atomic.Int32
	srv := wakingLegacy(1_000_000, &hits) // never wakes
	defer srv.Close()

	start := time.Now()
	rec := httptest.NewRecorder()
	proxyTo(t, srv, fastRetry(50*time.Millisecond)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tracks", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want the last 502 once the window is spent", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("gave up after %s, want about 50ms", elapsed)
	}
	if hits.Load() < 2 {
		t.Errorf("only %d attempt(s), expected retries within the window", hits.Load())
	}
}

func TestOtherStatusesAreNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	proxyTo(t, srv, fastRetry(time.Second)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nope", nil))
	if rec.Code != http.StatusNotFound || hits.Load() != 1 {
		t.Errorf("got %d after %d attempts, want 404 after 1", rec.Code, hits.Load())
	}
}

func TestClientCancellationStopsRetrying(t *testing.T) {
	var hits atomic.Int32
	srv := wakingLegacy(1_000_000, &hits)
	defer srv.Close()

	h := proxyTo(t, srv, func(rt *retryTransport) {
		rt.window = time.Minute
		rt.initialBackoff = 20 * time.Millisecond
		rt.maxBackoff = 20 * time.Millisecond
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/tracks", nil).WithContext(ctx))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("kept retrying for %s after the client left", elapsed)
	}
}
