package shadow

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type chanRecorder chan Result

func (c chanRecorder) RecordComparison(_ context.Context, r Result) error {
	c <- r
	return nil
}

type countingObserver struct {
	mu     sync.Mutex
	counts map[string]int
}

func (o *countingObserver) ObserveShadow(_, outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.counts == nil {
		o.counts = map[string]int{}
	}
	o.counts[outcome]++
}

func (o *countingObserver) get(outcome string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.counts[outcome]
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)) }

func job(body string) Job {
	return Job{
		Route:        "/api/tracks",
		Req:          httptest.NewRequest(http.MethodGet, "/api/tracks", nil),
		LegacyStatus: http.StatusOK,
		LegacyBody:   []byte(body),
	}
}

func runOne(t *testing.T, candidate http.Handler, j Job, timeout time.Duration) Result {
	t.Helper()
	rec := make(chanRecorder, 1)
	r := NewRunner(candidate, rec, &countingObserver{}, timeout, quiet())
	r.Start(1, 1)
	if !r.Submit(j) {
		t.Fatal("job rejected")
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	return <-rec
}

func respond(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func TestStatusAndBodyMismatch(t *testing.T) {
	res := runOne(t, respond(http.StatusInternalServerError, `{"error":"internal error"}`), job(`{"data":[]}`), time.Second)

	want := []string{
		"status: legacy=200 go=500",
		`$.data: legacy=[] go=<missing>`,
		`$.error: legacy=<missing> go="internal error"`,
	}
	if res.Match || strings.Join(res.Diffs, "|") != strings.Join(want, "|") {
		t.Errorf("diffs = %q", res.Diffs)
	}
}

func TestNonJSONFallsBackToBytes(t *testing.T) {
	same := runOne(t, respond(http.StatusOK, "plain text"), job("plain text"), time.Second)
	if !same.Match || len(same.Diffs) != 0 {
		t.Errorf("identical non-JSON bodies should match: %+v", same)
	}
	diff := runOne(t, respond(http.StatusOK, "<html>"), job(`{"data":[]}`), time.Second)
	if diff.Match || len(diff.Diffs) != 1 || !strings.HasPrefix(diff.Diffs[0], "body: not comparable as JSON") {
		t.Errorf("diffs = %q", diff.Diffs)
	}
}

func TestPanicBecomesErrorResult(t *testing.T) {
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("nil map") })
	res := runOne(t, boom, job(`{}`), time.Second)

	if res.Match || !strings.Contains(res.Error, "panicked: nil map") {
		t.Errorf("result = %+v", res)
	}
	if res.Diffs == nil {
		t.Error("diffs must never be nil")
	}
}

func TestTimeoutBecomesErrorResult(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		_, _ = w.Write([]byte(`{}`))
	})
	res := runOne(t, slow, job(`{}`), 20*time.Millisecond)
	if res.Match || !strings.Contains(res.Error, "shadow timeout") {
		t.Errorf("result = %+v", res)
	}
}

func TestOversizedGoResponseIsAnError(t *testing.T) {
	huge := respond(http.StatusOK, `"`+strings.Repeat("x", MaxBody)+`"`)
	res := runOne(t, huge, job(`"x"`), time.Second)
	if res.Match || !strings.Contains(res.Error, "body limit") {
		t.Errorf("result error = %q", res.Error)
	}
}

func TestSubmitNeverBlocksAndCountsDrops(t *testing.T) {
	obs := &countingObserver{}
	r := NewRunner(respond(http.StatusOK, `{}`), make(chanRecorder, 10), obs, time.Second, quiet())
	r.Start(0, 1) // no workers: the queue fills after one job

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			r.Submit(job(`{}`))
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Submit blocked on a full queue")
	}
	if got := obs.get(OutcomeDropped); got != 4 {
		t.Errorf("dropped = %d, want 4", got)
	}
}

func TestSubmitAfterCloseIsRejected(t *testing.T) {
	r := NewRunner(respond(http.StatusOK, `{}`), make(chanRecorder, 1), &countingObserver{}, time.Second, quiet())
	r.Start(1, 1)
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Submit(job(`{}`)) {
		t.Error("Submit accepted a job after Close")
	}
}

func TestCloseHonoursDeadline(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	stuck := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block })

	r := NewRunner(stuck, make(chanRecorder, 1), &countingObserver{}, time.Hour, quiet())
	r.Start(1, 1)
	r.Submit(job(`{}`))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.Close(ctx); err == nil {
		t.Error("Close should give up when its context expires")
	}
}
