package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestCountersAreSortedAndLabelled(t *testing.T) {
	r := NewRegistry()
	r.ObserveRequest("/api/tracks", "legacy", 200, time.Millisecond)
	r.ObserveRequest("/api/programs", "go", 200, time.Millisecond)
	r.ObserveRequest("/api/tracks", "legacy", 200, time.Millisecond)
	r.ObserveRequest("/api/tracks", "go", 500, time.Millisecond)
	r.ObserveShadow("/api/tracks", "mismatch")
	r.ObserveShadow("/api/now-playing", "match")
	r.ObserveShadow("/api/tracks", "mismatch")

	out := render(t, r)
	want := []string{
		`proxy_requests_total{route="/api/programs",backend="go",code="200"} 1`,
		`proxy_requests_total{route="/api/tracks",backend="go",code="500"} 1`,
		`proxy_requests_total{route="/api/tracks",backend="legacy",code="200"} 2`,
		`shadow_comparisons_total{route="/api/now-playing",outcome="match"} 1`,
		`shadow_comparisons_total{route="/api/tracks",outcome="mismatch"} 2`,
	}
	last := -1
	for _, line := range want {
		i := strings.Index(out, line+"\n")
		if i < 0 {
			t.Fatalf("missing line %q in:\n%s", line, out)
		}
		if i < last {
			t.Errorf("line %q is out of order", line)
		}
		last = i
	}
	for _, family := range []string{"proxy_requests_total counter", "proxy_request_duration_seconds histogram", "shadow_comparisons_total counter"} {
		if !strings.Contains(out, "# TYPE "+family+"\n") {
			t.Errorf("missing TYPE for %s", family)
		}
	}
}

func TestHistogramIsCumulative(t *testing.T) {
	r := NewRegistry()
	for _, d := range []time.Duration{3 * time.Millisecond, 40 * time.Millisecond, 1200 * time.Millisecond, 90 * time.Second} {
		r.ObserveRequest("/api/tracks", "legacy", 200, d)
	}
	out := render(t, r)

	labels := `route="/api/tracks",backend="legacy"`
	for _, line := range []string{
		`proxy_request_duration_seconds_bucket{` + labels + `,le="0.005"} 1`,
		`proxy_request_duration_seconds_bucket{` + labels + `,le="0.025"} 1`,
		`proxy_request_duration_seconds_bucket{` + labels + `,le="0.05"} 2`,
		`proxy_request_duration_seconds_bucket{` + labels + `,le="2.5"} 3`,
		`proxy_request_duration_seconds_bucket{` + labels + `,le="60"} 3`,
		`proxy_request_duration_seconds_bucket{` + labels + `,le="+Inf"} 4`, // 90s only lands in +Inf
		`proxy_request_duration_seconds_sum{` + labels + `} 91.24`,          // prefix: float addition
		`proxy_request_duration_seconds_count{` + labels + `} 4`,
	} {
		if !strings.Contains(out, line) {
			t.Errorf("missing %q in:\n%s", line, out)
		}
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	r := NewRegistry()
	r.ObserveShadow("/weird\"route\\\n", "match")
	if out := render(t, r); !strings.Contains(out, `route="/weird\"route\\\n"`) {
		t.Errorf("label not escaped:\n%s", out)
	}
}

func TestServeHTTPContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRegistry().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestConcurrentObservations(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				r.ObserveRequest("/api/tracks", "go", 200, time.Millisecond)
				r.ObserveShadow("/api/tracks", "match")
			}
		}()
	}
	wg.Wait()
	if out := render(t, r); !strings.Contains(out, `proxy_requests_total{route="/api/tracks",backend="go",code="200"} 8000`) {
		t.Errorf("lost updates:\n%s", out)
	}
}
