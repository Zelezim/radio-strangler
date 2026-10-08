// Package proxy is the Strangler Fig facade: it receives every client request and sends it to
// the backend selected by the route's migration rule.
package proxy

import (
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

// Proxy routes each request to the legacy or the Go implementation.
type Proxy struct {
	rules  RuleMatcher
	legacy http.Handler
	log    *slog.Logger
}

// New builds the facade. legacy is usually the handler returned by NewLegacy.
func New(rules RuleMatcher, legacy http.Handler, log *slog.Logger) *Proxy {
	return &Proxy{rules: rules, legacy: legacy, log: log}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rule := p.rules.Match(r.URL.Path)
	backend := rule.Backend(ClientKey(r))

	// The Go API does not exist yet, so a "go" decision is still answered by legacy. Routing is
	// already computed for real so the switch to Go is a one-line change once handlers exist.
	if backend == routing.BackendGo {
		backend = routing.BackendLegacy
	}

	w.Header().Set(httpx.ServedByHeader, string(backend))
	w.Header().Set(RouteModeHeader, string(rule.Mode))
	p.legacy.ServeHTTP(w, r)
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
