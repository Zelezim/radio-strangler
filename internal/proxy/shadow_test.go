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

	"github.com/Zelezim/radio-strangler/internal/httpx"
	"github.com/Zelezim/radio-strangler/internal/routing"
	"github.com/Zelezim/radio-strangler/internal/shadow"
)

type chanRecorder chan shadow.Result

func (c chanRecorder) RecordComparison(_ context.Context, r shadow.Result) error {
	c <- r
	return nil
}

type nopObserver struct{}

func (nopObserver) ObserveShadow(string, string) {}

// shadowEnv wires a fake PHP server (through the real reverse proxy), a fake Go API and a real
// shadow runner. Counters are atomic because handlers run on other goroutines (-race).
type shadowEnv struct {
	legacyHits    atomic.Int32
	candidateHits atomic.Int32
	results       chanRecorder
	runner        *shadow.Runner
	handler       http.Handler
}

func newShadowEnv(t *testing.T, rules []routing.Rule, legacyBody string, candidate http.Handler) *shadowEnv {
	t.Helper()
	env := &shadowEnv{results: make(chanRecorder, 10)}

	php := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.legacyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(legacyBody))
	}))
	t.Cleanup(php.Close)
	target, _ := url.Parse(php.URL)

	countingCandidate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.candidateHits.Add(1)
		candidate.ServeHTTP(w, r)
	})

	log := quietLogger()
	env.runner = shadow.NewRunner(countingCandidate, env.results, nopObserver{}, time.Second, log)
	env.runner.Start(2, 10)

	table := routing.NewTable()
	table.Replace(rules)
	env.handler = httpx.Chain(New(table, NewLegacy(target, 5*time.Second, log), countingCandidate, env.runner, log), httpx.RequestID)
	return env
}

func (e *shadowEnv) do(method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// finish waits for every queued comparison and returns the recorded results.
func (e *shadowEnv) finish(t *testing.T) []shadow.Result {
	t.Helper()
	if err := e.runner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(e.results)
	var out []shadow.Result
	for r := range e.results {
		out = append(out, r)
	}
	return out
}

func goBody(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

var shadowTracks = []routing.Rule{{Route: "/api/tracks", Mode: routing.ModeShadow}}

func TestShadowServesLegacyAndRecordsMismatch(t *testing.T) {
	legacy := `{"data":[{"id":3,"title":"Station Ident #3","tags":[]}]}`
	env := newShadowEnv(t, shadowTracks, legacy, goBody(`{"data":[{"id":3,"title":"Station Ident #3","tags":null}]}`))

	rec := env.do(http.MethodGet, "/api/tracks")
	if rec.Body.String() != legacy {
		t.Errorf("client got %q, want the legacy body", rec.Body.String())
	}
	if rec.Header().Get(httpx.ServedByHeader) != "legacy" || rec.Header().Get(RouteModeHeader) != "shadow" {
		t.Errorf("headers = %v", rec.Header())
	}

	results := env.finish(t)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Match || len(r.Diffs) != 1 || r.Diffs[0] != "$.data[0].tags: legacy=[] go=null" {
		t.Errorf("result = %+v", r)
	}
	if r.Route != "/api/tracks" || r.Method != http.MethodGet || r.Path != "/api/tracks" ||
		r.LegacyStatus != http.StatusOK || r.GoStatus != http.StatusOK {
		t.Errorf("metadata = %+v", r)
	}
}

func TestShadowEquivalentBodiesMatch(t *testing.T) {
	rules := []routing.Rule{{Route: "/api/now-playing", Mode: routing.ModeShadow, IgnoreFields: []string{"generated_at"}}}
	env := newShadowEnv(t, rules,
		`{"track":{"url":"a\/b","n":1},"generated_at":"2026-10-08T10:00:00Z"}`,
		goBody(`{"generated_at":"2026-10-08T10:00:01Z","track":{"n":1.0,"url":"a/b"}}`+"\n"))

	env.do(http.MethodGet, "/api/now-playing")
	results := env.finish(t)
	if len(results) != 1 || !results[0].Match || len(results[0].Diffs) != 0 || results[0].Diffs == nil {
		t.Errorf("results = %+v", results)
	}
}

func TestGoModeSkipsLegacyAndShadow(t *testing.T) {
	env := newShadowEnv(t, []routing.Rule{{Route: "/api/tracks", Mode: routing.ModeGo}}, `{}`, goBody(`{"from":"go"}`))

	rec := env.do(http.MethodGet, "/api/tracks")
	results := env.finish(t)

	if rec.Body.String() != `{"from":"go"}` || env.legacyHits.Load() != 0 || env.candidateHits.Load() != 1 || len(results) != 0 {
		t.Errorf("body=%q legacy=%d candidate=%d results=%d",
			rec.Body.String(), env.legacyHits.Load(), env.candidateHits.Load(), len(results))
	}
}

func TestRouteWithoutRuleGoesToLegacyOnly(t *testing.T) {
	env := newShadowEnv(t, shadowTracks, `{"data":[]}`, goBody(`{"data":[]}`))

	rec := env.do(http.MethodGet, "/api/programs")
	results := env.finish(t)

	if rec.Header().Get(RouteModeHeader) != "legacy" || env.legacyHits.Load() != 1 || env.candidateHits.Load() != 0 || len(results) != 0 {
		t.Errorf("mode=%q legacy=%d candidate=%d results=%d",
			rec.Header().Get(RouteModeHeader), env.legacyHits.Load(), env.candidateHits.Load(), len(results))
	}
}

func TestPostIsNeverShadowed(t *testing.T) {
	env := newShadowEnv(t, shadowTracks, `{"error":"method not allowed"}`, goBody(`{}`))

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		env.do(method, "/api/tracks")
	}
	results := env.finish(t)

	if env.legacyHits.Load() != 4 || env.candidateHits.Load() != 0 || len(results) != 0 {
		t.Errorf("legacy=%d candidate=%d results=%d", env.legacyHits.Load(), env.candidateHits.Load(), len(results))
	}
}

func TestClientDisconnectDoesNotCancelComparison(t *testing.T) {
	release := make(chan struct{})
	var ctxErr atomic.Value
	var requestID atomic.Value
	candidate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // run only after the client has gone away
		ctxErr.Store(r.Context().Err() == nil)
		requestID.Store(r.Header.Get(httpx.RequestIDHeader))
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	env := newShadowEnv(t, shadowTracks, `{"data":[]}`, candidate)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/tracks", nil).WithContext(ctx)
	req.Header.Set(httpx.RequestIDHeader, "client-req-1")
	env.handler.ServeHTTP(httptest.NewRecorder(), req)
	cancel() // client disconnects right after its response
	close(release)

	results := env.finish(t)
	if len(results) != 1 || !results[0].Match || results[0].Error != "" {
		t.Fatalf("results = %+v", results)
	}
	if ok, _ := ctxErr.Load().(bool); !ok {
		t.Error("shadow request context was cancelled by the client")
	}
	if id, _ := requestID.Load().(string); id != "client-req-1" {
		t.Errorf("shadow request id = %q, want client-req-1", id)
	}
}

func TestOversizedLegacyBodyIsNotShadowed(t *testing.T) {
	big := `"` + strings.Repeat("x", shadow.MaxBody) + `"`
	env := newShadowEnv(t, shadowTracks, big, goBody(big))

	rec := env.do(http.MethodGet, "/api/tracks")
	results := env.finish(t)

	if rec.Body.Len() != len(big) {
		t.Errorf("client body truncated: %d of %d bytes", rec.Body.Len(), len(big))
	}
	if env.candidateHits.Load() != 0 || len(results) != 0 {
		t.Errorf("candidate=%d results=%d, want no shadow", env.candidateHits.Load(), len(results))
	}
}
