// Package proxy is the Strangler Fig facade: it receives every client request and sends it to
// the backend selected by the route's migration rule.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/Zelezim/radio-strangler/internal/httpx"
	"github.com/Zelezim/radio-strangler/internal/routing"
	"github.com/Zelezim/radio-strangler/internal/shadow"
)

const (
	// RouteModeHeader exposes the rule's mode, so clients and demos can see the migration state.
	RouteModeHeader = "X-Route-Mode"
	// ClientIDHeader lets a client pin its canary bucket explicitly (mobile apps, tests).
	ClientIDHeader = "X-Client-ID"
)

// RuleMatcher resolves the rule for a request path.
type RuleMatcher interface {
	Match(path string) routing.Rule
}

// ShadowSubmitter accepts comparison jobs without blocking.
type ShadowSubmitter interface {
	Submit(job shadow.Job) bool
}

// RequestObserver is told about every proxied request (metrics hook).
type RequestObserver interface {
	ObserveRequest(route, backend string, status int, d time.Duration)
}

// UnmatchedRoute is the route label for requests that matched no rule.
const UnmatchedRoute = "unmatched"

// Proxy routes each request to the legacy or the Go implementation.
type Proxy struct {
	rules     RuleMatcher
	legacy    http.Handler
	candidate http.Handler
	shadow    ShadowSubmitter
	obs       RequestObserver
	log       *slog.Logger
}

// New builds the facade. legacy is usually the handler returned by NewLegacy; candidate is the
// in-process Go API that is replacing it; sh receives the comparisons and obs the per-request
// measurements (either may be nil).
func New(rules RuleMatcher, legacy, candidate http.Handler, sh ShadowSubmitter, obs RequestObserver, log *slog.Logger) *Proxy {
	return &Proxy{rules: rules, legacy: legacy, candidate: candidate, shadow: sh, obs: obs, log: log}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rule := p.rules.Match(r.URL.Path)
	backend := rule.Backend(ClientKey(r))

	if p.obs != nil {
		start := time.Now()
		sw := &httpx.StatusWriter{ResponseWriter: w}
		w = sw
		defer func() {
			status := sw.Status
			if status == 0 {
				status = http.StatusOK
			}
			route := rule.Route
			if route == "" {
				route = UnmatchedRoute
			}
			p.obs.ObserveRequest(route, string(backend), status, time.Since(start))
		}()
	}

	w.Header().Set(httpx.ServedByHeader, string(backend))
	w.Header().Set(RouteModeHeader, string(rule.Mode))

	if backend == routing.BackendGo {
		// Same process, plain function call: no network hop, so "go" mode costs nothing extra.
		p.candidate.ServeHTTP(w, r)
		return
	}
	// Only safe methods are replayed: shadowing a POST would execute the write twice, once in
	// each implementation, against the same database.
	if rule.Mode == routing.ModeShadow && r.Method == http.MethodGet && p.shadow != nil {
		p.serveShadow(w, r, rule)
		return
	}
	p.legacy.ServeHTTP(w, r)
}

// serveShadow answers from legacy and hands a copy of the exchange to the shadow runner.
func (p *Proxy) serveShadow(w http.ResponseWriter, r *http.Request, rule routing.Rule) {
	// Cloned before legacy sees the request: the reverse proxy mutates its input. The context
	// keeps its values (request id) but not its cancellation, because the client disconnecting
	// right after its response must not cancel the comparison that runs afterwards.
	replay := r.Clone(context.WithoutCancel(r.Context()))
	replay.Body = http.NoBody

	cw := &captureWriter{ResponseWriter: w, limit: shadow.MaxBody}
	start := time.Now()
	p.legacy.ServeHTTP(cw, r)
	latency := time.Since(start)

	if cw.truncated {
		p.log.Debug("shadow skipped: legacy body over limit", "route", rule.Route, "path", r.URL.Path)
		return
	}
	p.shadow.Submit(shadow.Job{
		Route:         rule.Route,
		IgnoreFields:  rule.IgnoreFields,
		Req:           replay,
		LegacyStatus:  cw.statusCode(),
		LegacyBody:    cw.buf.Bytes(),
		LegacyLatency: latency,
	})
}

// captureWriter streams the response to the client unchanged while keeping a bounded copy.
type captureWriter struct {
	http.ResponseWriter
	status    int
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 && code >= 200 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if !c.truncated {
		if c.buf.Len()+len(b) > c.limit {
			// Too big to compare: stop copying and free what we have; the client is unaffected.
			c.truncated = true
			c.buf = bytes.Buffer{}
		} else {
			c.buf.Write(b)
		}
	}
	return c.ResponseWriter.Write(b)
}

// Flush keeps streamed legacy responses streaming through the capture.
func (c *captureWriter) Flush() {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (c *captureWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

func (c *captureWriter) statusCode() int {
	if c.status == 0 {
		return http.StatusOK
	}
	return c.status
}

// ClientKey identifies a client for canary bucketing only. Every source here is client-controlled
// or spoofable, which is fine for spreading load and wrong for anything security related.
func ClientKey(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get(ClientIDHeader)); id != "" {
		return id
	}
	// Behind a load balancer (Render) RemoteAddr is the balancer; the first X-Forwarded-For
	// entry is the original client.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := strings.TrimSpace(first); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// NewLegacy returns a reverse proxy to the legacy API.
//
// timeout bounds the wait for response headers. It is generous on purpose: a free-tier legacy
// service can cold start in tens of seconds, and a slow answer beats a 502 during the demo.
func NewLegacy(target *url.URL, timeout time.Duration, log *slog.Logger) *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		// Every request goes to the same host; the default of 2 idle connections per host would
		// force a new TCP/TLS handshake under any real concurrency.
		MaxIdleConnsPerHost: 32,
		ForceAttemptHTTP2:   true,
	}

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL also rewrites Host to the target's. Platforms like Render route by Host, so
			// forwarding the client's Host would hit the wrong service.
			pr.SetURL(target)
			// Rewrite starts with X-Forwarded-For stripped; restoring it keeps the chain from any
			// load balancer in front of us, and SetXForwarded appends the direct peer.
			pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
			pr.SetXForwarded()
		},
		Transport: transport,
		ErrorLog:  slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			level := slog.LevelWarn
			// The client gave up first; that is not a backend problem worth a warning.
			if errors.Is(err, context.Canceled) {
				level = slog.LevelDebug
			}
			log.Log(r.Context(), level, "legacy backend error",
				"err", err,
				"path", r.URL.Path,
				"request_id", httpx.RequestIDFrom(r.Context()),
			)
			httpx.Error(w, http.StatusBadGateway, "legacy backend unavailable")
		},
	}
}
