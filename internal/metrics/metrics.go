// Package metrics exposes proxy and shadow metrics in the Prometheus text format.
//
// It is hand-written instead of using prometheus/client_golang on purpose: the project allows no
// dependencies beyond the standard library and lib/pq, and three metric families do not justify
// one. The exposition format is plain text and stable, so a small registry is enough.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBuckets span a cached Go answer (5ms) up to a cold-starting legacy backend (60s).
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type requestKey struct {
	route, backend, code string
}

type routeBackend struct {
	route, backend string
}

type shadowKey struct {
	route, outcome string
}

type histogram struct {
	buckets []uint64 // per-bucket (non-cumulative) counts; cumulated when written
	count   uint64
	sum     float64
}

// Registry holds every metric. It implements proxy.RequestObserver and shadow.Observer.
//
// The route label is always the matched rule's route (or "unmatched"), never the raw request
// path: paths contain ids and arbitrary strings from clients, and a label per distinct path
// would grow the number of series without bound.
type Registry struct {
	mu        sync.Mutex
	buckets   []float64
	requests  map[requestKey]uint64
	durations map[routeBackend]*histogram
	shadow    map[shadowKey]uint64
}

// NewRegistry returns an empty registry using DefaultBuckets.
func NewRegistry() *Registry {
	return &Registry{
		buckets:   DefaultBuckets,
		requests:  make(map[requestKey]uint64),
		durations: make(map[routeBackend]*histogram),
		shadow:    make(map[shadowKey]uint64),
	}
}

// ObserveRequest records one proxied request.
func (r *Registry) ObserveRequest(route, backend string, status int, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests[requestKey{route, backend, strconv.Itoa(status)}]++

	k := routeBackend{route, backend}
	h := r.durations[k]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(r.buckets))}
		r.durations[k] = h
	}
	secs := d.Seconds()
	h.count++
	h.sum += secs
	for i, le := range r.buckets {
		if secs <= le {
			h.buckets[i]++
			break
		}
	}
}

// ObserveShadow records the outcome of one shadow job.
func (r *Registry) ObserveShadow(route, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shadow[shadowKey{route, outcome}]++
}

// ServeHTTP writes the metrics in the Prometheus text exposition format.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = r.Write(w)
}

// Write renders every family with series sorted by labels, so output is deterministic.
func (r *Registry) Write(out io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder

	b.WriteString("# HELP proxy_requests_total Requests answered by the proxy, by rule route, backend and status code.\n")
	b.WriteString("# TYPE proxy_requests_total counter\n")
	reqKeys := sortedKeys(r.requests, func(a, c requestKey) int {
		return cmpStrings(a.route, c.route, a.backend, c.backend, a.code, c.code)
	})
	for _, k := range reqKeys {
		fmt.Fprintf(&b, "proxy_requests_total{route=%s,backend=%s,code=%s} %d\n",
			quote(k.route), quote(k.backend), quote(k.code), r.requests[k])
	}

	b.WriteString("# HELP proxy_request_duration_seconds Time to answer a proxied request, by rule route and backend.\n")
	b.WriteString("# TYPE proxy_request_duration_seconds histogram\n")
	durKeys := sortedKeys(r.durations, func(a, c routeBackend) int {
		return cmpStrings(a.route, c.route, a.backend, c.backend)
	})
	for _, k := range durKeys {
		h := r.durations[k]
		labels := fmt.Sprintf("route=%s,backend=%s", quote(k.route), quote(k.backend))
		var cumulative uint64
		for i, le := range r.buckets {
			cumulative += h.buckets[i]
			fmt.Fprintf(&b, "proxy_request_duration_seconds_bucket{%s,le=%q} %d\n", labels, formatFloat(le), cumulative)
		}
		fmt.Fprintf(&b, "proxy_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", labels, h.count)
		fmt.Fprintf(&b, "proxy_request_duration_seconds_sum{%s} %s\n", labels, formatFloat(h.sum))
		fmt.Fprintf(&b, "proxy_request_duration_seconds_count{%s} %d\n", labels, h.count)
	}

	b.WriteString("# HELP shadow_comparisons_total Shadow comparisons, by rule route and outcome (match, mismatch, error, dropped).\n")
	b.WriteString("# TYPE shadow_comparisons_total counter\n")
	shKeys := sortedKeys(r.shadow, func(a, c shadowKey) int {
		return cmpStrings(a.route, c.route, a.outcome, c.outcome)
	})
	for _, k := range shKeys {
		fmt.Fprintf(&b, "shadow_comparisons_total{route=%s,outcome=%s} %d\n",
			quote(k.route), quote(k.outcome), r.shadow[k])
	}

	_, err := io.WriteString(out, b.String())
	return err
}

func sortedKeys[K comparable, V any](m map[K]V, cmp func(a, b K) int) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, cmp)
	return keys
}

// cmpStrings compares pairs (a1,b1), (a2,b2)... in order, like a tuple comparison.
func cmpStrings(pairs ...string) int {
	for i := 0; i+1 < len(pairs); i += 2 {
		if c := strings.Compare(pairs[i], pairs[i+1]); c != 0 {
			return c
		}
	}
	return 0
}

// quote escapes a label value as the exposition format requires: backslash, quote, newline.
func quote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return `"` + v + `"`
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
