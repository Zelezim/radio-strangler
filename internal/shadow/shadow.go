// Package shadow replays legacy-served requests against the Go implementation in the background
// and records whether both answered the same.
//
// Guarantees, in order of importance:
//   - The client never waits for shadow work: the legacy response is already sent when a job is
//     submitted, and Submit never blocks (a full queue drops the job and counts it).
//   - Shadow traffic can never take the service down: a fixed pool of workers and a bounded queue
//     cap the extra CPU, memory and database load no matter how much traffic arrives.
//   - A panic in the new code does not escape: it is recovered and recorded as an error result.
package shadow

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/Zelezim/radio-strangler/internal/diff"
	"github.com/Zelezim/radio-strangler/internal/httpx"
)

// MaxBody is the largest response either side may produce to be compared. Larger bodies are
// skipped rather than buffered: memory per job stays bounded.
const MaxBody = 1 << 20 // 1 MiB

// Outcomes reported to the Observer.
const (
	OutcomeMatch    = "match"
	OutcomeMismatch = "mismatch"
	OutcomeError    = "error"
	OutcomeDropped  = "dropped"
)

// Job is one legacy exchange to replay against Go.
type Job struct {
	Route         string
	IgnoreFields  []string
	Req           *http.Request // must not carry a body and must not be cancelled by the client
	LegacyStatus  int
	LegacyBody    []byte
	LegacyLatency time.Duration
}

// Result is the outcome of one comparison.
type Result struct {
	ID           int64     `json:"id,omitempty"` // set when read back from storage
	Route        string    `json:"route"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	LegacyStatus int       `json:"legacy_status"`
	GoStatus     int       `json:"go_status"`
	Match        bool      `json:"match"`
	Diffs        []string  `json:"diffs"`
	LegacyMS     int64     `json:"legacy_ms"`
	GoMS         int64     `json:"go_ms"`
	Error        string    `json:"error"`
	CreatedAt    time.Time `json:"created_at"`
}

// Recorder persists results.
type Recorder interface {
	RecordComparison(ctx context.Context, r Result) error
}

// Observer is told the outcome of every job, including dropped ones (metrics hook).
type Observer interface {
	ObserveShadow(route, outcome string)
}

// Runner executes jobs on a fixed pool of workers.
type Runner struct {
	candidate http.Handler
	rec       Recorder
	obs       Observer
	timeout   time.Duration
	log       *slog.Logger

	// mu makes "check closed, then send" atomic with Close, so Submit can never send on a
	// closed channel. Submit only takes the read lock, so submitters do not contend.
	mu     sync.RWMutex
	closed bool
	jobs   chan Job
	wg     sync.WaitGroup
}

// NewRunner creates a runner; call Start before submitting.
func NewRunner(candidate http.Handler, rec Recorder, obs Observer, timeout time.Duration, log *slog.Logger) *Runner {
	return &Runner{candidate: candidate, rec: rec, obs: obs, timeout: timeout, log: log}
}

// Start launches workers goroutines consuming a queue of the given size.
func (s *Runner) Start(workers, queue int) {
	s.jobs = make(chan Job, queue)
	for i := 0; i < workers; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for job := range s.jobs {
				s.run(job)
			}
		}()
	}
}

// Submit enqueues a job without ever blocking. It reports whether the job was accepted.
func (s *Runner) Submit(job Job) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.jobs == nil {
		return false
	}
	select {
	case s.jobs <- job:
		return true
	default:
		// Losing a comparison is acceptable; slowing down a real client is not.
		s.obs.ObserveShadow(job.Route, OutcomeDropped)
		return false
	}
}

// Close stops accepting jobs and waits for queued ones to finish, or for ctx to expire.
func (s *Runner) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		if s.jobs != nil {
			close(s.jobs)
		}
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("shadow runner: pending jobs abandoned: %w", ctx.Err())
	}
}

func (s *Runner) run(job Job) {
	// Derived from the request context, which the proxy already detached from the client: the
	// request id and other values survive, cancellation does not.
	ctx, cancel := context.WithTimeout(job.Req.Context(), s.timeout)
	defer cancel()
	req := job.Req.WithContext(ctx)

	res := Result{
		Route:        job.Route,
		Method:       req.Method,
		Path:         req.URL.Path,
		LegacyStatus: job.LegacyStatus,
		LegacyMS:     job.LegacyLatency.Milliseconds(),
		CreatedAt:    time.Now().UTC(),
	}

	w := newBufferWriter(MaxBody)
	start := time.Now()
	err := s.serveCandidate(w, req)
	res.GoMS = time.Since(start).Milliseconds()
	res.GoStatus = w.statusCode()

	switch {
	case err != nil:
		res.Error = err.Error()
	case ctx.Err() != nil:
		// The handler is in-process and cannot be preempted; the deadline only cancels its
		// database calls, so whatever it produced afterwards is not a fair comparison.
		res.Error = fmt.Sprintf("go handler exceeded shadow timeout of %s", s.timeout)
	case w.truncated:
		res.Error = "go response exceeds shadow body limit"
	default:
		res.Diffs = compare(job, res.GoStatus, w.body.Bytes())
		res.Match = len(res.Diffs) == 0
	}
	if res.Diffs == nil {
		res.Diffs = []string{}
	}

	outcome := OutcomeMatch
	switch {
	case res.Error != "":
		outcome = OutcomeError
	case !res.Match:
		outcome = OutcomeMismatch
	}

	s.record(job.Req.Context(), res)
	s.obs.ObserveShadow(job.Route, outcome)

	attrs := []any{
		"route", res.Route,
		"path", res.Path,
		"legacy_ms", res.LegacyMS,
		"go_ms", res.GoMS,
		"request_id", httpx.RequestIDFrom(ctx),
	}
	switch outcome {
	case OutcomeMismatch:
		s.log.Info("shadow mismatch", append(attrs, "diffs", res.Diffs)...)
	case OutcomeError:
		s.log.Warn("shadow error", append(attrs, "err", res.Error)...)
	default:
		s.log.Debug("shadow match", attrs...)
	}
}

func (s *Runner) serveCandidate(w http.ResponseWriter, r *http.Request) (err error) {
	defer func() {
		if v := recover(); v != nil {
			s.log.Error("go handler panicked in shadow", "panic", v, "path", r.URL.Path, "stack", string(debug.Stack()))
			err = fmt.Errorf("go handler panicked: %v", v)
		}
	}()
	s.candidate.ServeHTTP(w, r)
	return nil
}

func (s *Runner) record(parent context.Context, res Result) {
	// Own short deadline: the shadow timeout may already be spent, and a slow database must not
	// pile up workers.
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	if err := s.rec.RecordComparison(ctx, res); err != nil {
		s.log.Warn("failed to record shadow comparison", "route", res.Route, "err", err)
	}
}

// compare reports status and body differences. Non-JSON bodies fall back to exact bytes.
func compare(job Job, goStatus int, goBody []byte) []string {
	var diffs []string
	if job.LegacyStatus != goStatus {
		diffs = append(diffs, fmt.Sprintf("status: legacy=%d go=%d", job.LegacyStatus, goStatus))
	}
	bodyDiffs, err := diff.JSON(job.LegacyBody, goBody, diff.Options{IgnoreFields: job.IgnoreFields})
	if err != nil {
		if !bytes.Equal(job.LegacyBody, goBody) {
			diffs = append(diffs, fmt.Sprintf("body: not comparable as JSON (%v) and bytes differ", err))
		}
		return diffs
	}
	return append(diffs, bodyDiffs...)
}

// bufferWriter is an in-memory ResponseWriter for the replayed request, capped at limit bytes.
type bufferWriter struct {
	header    http.Header
	status    int
	body      bytes.Buffer
	limit     int
	truncated bool
}

func newBufferWriter(limit int) *bufferWriter {
	return &bufferWriter{header: make(http.Header), limit: limit}
}

func (w *bufferWriter) Header() http.Header { return w.header }

func (w *bufferWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
}

func (w *bufferWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if remaining := w.limit - w.body.Len(); len(b) > remaining {
		w.body.Write(b[:max(remaining, 0)])
		w.truncated = true
		// Report success anyway: the handler under test must not see a different world.
		return len(b), nil
	}
	return w.body.Write(b)
}

func (w *bufferWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
