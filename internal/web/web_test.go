package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestSite(t *testing.T, opts Options) *http.ServeMux {
	t.Helper()
	site, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	site.Register(mux)
	// Stand-in for the proxy: anything the UI does not own must reach it.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	return mux
}

var faith = Options{
	StationName:       "Faith FM Australia",
	StreamURL:         "https://stream1.faithfm.com.au/FaithFM.mp3",
	StreamURLFallback: "https://stream1.faithfm.com.au/FaithFM.ogg",
	HeroImageURL:      "https://cdn.faithfm.com.au/hero.jpg",
	RepoURL:           "https://github.com/Zelezim/radio-strangler",
}

func get(mux http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestPagesServeTheSameDocument(t *testing.T) {
	mux := newTestSite(t, faith)
	var first string
	for _, p := range Pages {
		rec := get(mux, p)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Header().Get("Content-Type"))
		}
		if first == "" {
			first = rec.Body.String()
		} else if rec.Body.String() != first {
			t.Errorf("%s serves a different document", p)
		}
	}
	for _, want := range []string{"Faith FM Australia", `src="https://stream1.faithfm.com.au/FaithFM.mp3"`, `type="audio/ogg"`, "not affiliated"} {
		if !strings.Contains(first, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestEverythingElseFallsThroughToTheProxy(t *testing.T) {
	mux := newTestSite(t, faith)
	for _, p := range []string{"/api/tracks", "/programs/1", "/trackslist", "/admin/routes", "/nope"} {
		if rec := get(mux, p); rec.Code != http.StatusTeapot {
			t.Errorf("%s: %d, want it routed to the proxy", p, rec.Code)
		}
	}
}

func TestStaticAssets(t *testing.T) {
	mux := newTestSite(t, faith)
	for path, ctype := range map[string]string{"/static/app.js": "javascript", "/static/style.css": "text/css"} {
		rec := get(mux, path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", path)
		}
	}
	if rec := get(mux, "/static/missing.js"); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset: %d, want 404", rec.Code)
	}
}

func TestAssetsAreCacheBusted(t *testing.T) {
	site, err := New(faith)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	site.Register(mux)

	page := get(mux, "/").Body.String()
	versioned := "/static/app.js?v=" + site.version
	if len(site.version) != 12 || !strings.Contains(page, versioned) || !strings.Contains(page, "/static/style.css?v="+site.version) {
		t.Fatalf("page does not link versioned assets (version %q)", site.version)
	}
	if cc := get(mux, versioned).Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("current version: Cache-Control = %q, want immutable", cc)
	}
	for _, path := range []string{"/static/app.js", "/static/app.js?v=stale"} {
		if cc := get(mux, path).Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache", path, cc)
		}
	}
}

func TestContentSecurityPolicy(t *testing.T) {
	csp := get(newTestSite(t, faith), "/").Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"script-src 'self'",
		"media-src https://stream1.faithfm.com.au",
		"img-src 'self' data: https://cdn.faithfm.com.au",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
	if strings.Count(csp, "stream1.faithfm.com.au") != 1 {
		t.Errorf("stream origin should be listed once: %q", csp)
	}
}

func TestOptionalURLsCanBeDisabled(t *testing.T) {
	opts := faith
	opts.StreamURLFallback = ""
	opts.HeroImageURL = ""
	rec := get(newTestSite(t, opts), "/")
	if strings.Contains(rec.Body.String(), "audio/ogg") {
		t.Error("fallback source rendered although disabled")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data:;") {
		t.Errorf("CSP img-src should only allow self: %q", csp)
	}
}

func TestStationNameIsEscaped(t *testing.T) {
	opts := faith
	opts.StationName = `<script>alert(1)</script>`
	body := get(newTestSite(t, opts), "/").Body.String()
	if strings.Contains(body, "<script>alert(1)") {
		t.Error("station name was not HTML-escaped")
	}
}

func TestNewRequiresStationAndStream(t *testing.T) {
	if _, err := New(Options{StationName: "x"}); err == nil {
		t.Error("expected an error without a stream URL")
	}
}
