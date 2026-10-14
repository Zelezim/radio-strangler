// Package web serves the public showcase UI: a live radio player plus pages that read the
// station API through the strangler proxy and show which backend answered each call.
//
// The UI is new code, it never existed in the legacy system, so it is served by Go directly and
// is not subject to route rules. It only talks to the API like any other client would.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

//go:embed templates/index.html
var indexTemplate string

//go:embed static
var staticFS embed.FS

// Pages are the client-side routes. They all serve the same document: navigation happens in the
// browser (history.pushState), so the <audio> element is never torn down and playback survives
// moving between pages.
var Pages = []string{"/", "/programs", "/tracks"}

// Options configures the station shown by the UI.
type Options struct {
	StationName       string
	StreamURL         string
	StreamURLFallback string // optional
	HeroImageURL      string // optional
	RepoURL           string
}

// Site holds the rendered page and its security headers.
type Site struct {
	page    []byte
	csp     string
	version string
	static  http.Handler
}

// pageData is what the template sees: the options plus the asset version used for cache busting.
type pageData struct {
	Options
	AssetVersion string
}

// New renders the page once: its content only depends on configuration, so there is nothing to
// compute per request.
func New(opts Options) (*Site, error) {
	if opts.StationName == "" || opts.StreamURL == "" {
		return nil, fmt.Errorf("web: station name and stream URL are required")
	}
	tmpl, err := template.New("index").Parse(indexTemplate)
	if err != nil {
		return nil, fmt.Errorf("web: parse template: %w", err)
	}
	assets, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("web: static assets: %w", err)
	}
	version, err := assetVersion(assets)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, pageData{Options: opts, AssetVersion: version}); err != nil {
		return nil, fmt.Errorf("web: render template: %w", err)
	}
	csp, err := contentSecurityPolicy(opts)
	if err != nil {
		return nil, err
	}
	return &Site{
		page:    buf.Bytes(),
		csp:     csp,
		version: version,
		static:  http.StripPrefix("/static/", http.FileServerFS(assets)),
	}, nil
}

// assetVersion hashes every embedded asset. The page links them as /static/x?v=<version>, so a
// deploy that changes any asset changes every URL and browsers can never mix old and new files.
func assetVersion(assets fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("web: hash assets: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// Register mounts the pages and the static assets on mux. Patterns are exact ("/{$}") so every
// other path still falls through to the proxy.
func (s *Site) Register(mux *http.ServeMux) {
	for _, p := range Pages {
		pattern := "GET " + p
		if p == "/" {
			pattern = "GET /{$}"
		}
		mux.Handle(pattern, http.HandlerFunc(s.servePage))
	}
	mux.Handle("GET /static/", http.HandlerFunc(s.serveStatic))
}

func (s *Site) servePage(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", s.csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	// The page embeds configuration (stream URL); a stale copy after a redeploy would point the
	// player at the old stream, so browsers must revalidate.
	h.Set("Cache-Control", "no-cache")
	_, _ = w.Write(s.page)
}

func (s *Site) serveStatic(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A URL carrying the current version can never change content, so it is cached for a year.
	// Anything else (no or stale version) must be revalidated.
	if r.URL.Query().Get("v") == s.version {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	s.static.ServeHTTP(w, r)
}

// contentSecurityPolicy only allows what the page needs: its own scripts, styles and API calls,
// audio from the configured stream hosts and the hero image host. Any injected script or
// third-party resource is refused by the browser.
func contentSecurityPolicy(opts Options) (string, error) {
	media, err := origins(opts.StreamURL, opts.StreamURLFallback)
	if err != nil {
		return "", err
	}
	images, err := origins(opts.HeroImageURL)
	if err != nil {
		return "", err
	}
	directives := []string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self'",
		"connect-src 'self'",
		"img-src " + strings.Join(append([]string{"'self'", "data:"}, images...), " "),
		"media-src " + strings.Join(media, " "),
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}
	return strings.Join(directives, "; "), nil
}

// origins returns the distinct scheme://host of each non-empty URL.
func origins(raw ...string) ([]string, error) {
	var out []string
	for _, r := range raw {
		if r == "" {
			continue
		}
		u, err := url.Parse(r)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("web: invalid URL %q", r)
		}
		o := u.Scheme + "://" + u.Host
		if !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	return out, nil
}
