// Package api is the Go implementation of the station API. Its only requirement is to be
// indistinguishable from the legacy PHP API: same paths, same JSON, same error envelopes.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Zelezim/radio-strangler/internal/httpx"
	"github.com/Zelezim/radio-strangler/internal/radio"
)

// Repo is the data the API needs. It is declared here, at the consumer, so the API can be tested
// with an in-memory fake and does not depend on the database package.
type Repo interface {
	ListPrograms(ctx context.Context) ([]radio.Program, error)
	ListTracks(ctx context.Context) ([]radio.Track, error)
	NowPlaying(ctx context.Context) (*radio.Play, error)
}

type api struct {
	repo       Repo
	injectBugs bool
	log        *slog.Logger
	now        func() time.Time
}

// New returns the API handler. injectBugs reproduces a known migration bug on purpose (see
// normalizeTags) and must only be enabled for demos.
func New(repo Repo, injectBugs bool, log *slog.Logger) http.Handler {
	return newAPI(repo, injectBugs, log, time.Now)
}

func newAPI(repo Repo, injectBugs bool, log *slog.Logger, now func() time.Time) http.Handler {
	a := &api{repo: repo, injectBugs: injectBugs, log: log, now: now}

	mux := http.NewServeMux()
	// Patterns carry no method on purpose: ServeMux's built-in 405 is plain text, while the
	// contract requires the legacy JSON envelope, so onlyGET produces it instead.
	mux.Handle("/api/programs", onlyGET(a.programs))
	mux.Handle("/api/tracks", onlyGET(a.tracks))
	mux.Handle("/api/now-playing", onlyGET(a.nowPlaying))
	// Unknown paths are 404 for every method, matching the legacy check order (path, then method).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, http.StatusNotFound, "not found")
	})
	return mux
}

// onlyGET accepts GET and HEAD (net/http drops the body for HEAD) and answers anything else
// with the legacy 405.
func onlyGET(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet)
			httpx.Error(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h(w, r)
	})
}

type programsResponse struct {
	Data []radio.Program `json:"data"`
}

func (a *api) programs(w http.ResponseWriter, r *http.Request) {
	programs, err := a.repo.ListPrograms(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	// A nil slice would encode as null; PHP's empty array encodes as [].
	if programs == nil {
		programs = []radio.Program{}
	}
	httpx.WriteJSON(w, http.StatusOK, programsResponse{Data: programs})
}

type tracksResponse struct {
	Data []radio.Track `json:"data"`
}

func (a *api) tracks(w http.ResponseWriter, r *http.Request) {
	tracks, err := a.repo.ListTracks(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if tracks == nil {
		tracks = []radio.Track{}
	}
	for i := range tracks {
		tracks[i].Tags = a.normalizeTags(tracks[i].Tags)
	}
	httpx.WriteJSON(w, http.StatusOK, tracksResponse{Data: tracks})
}

// nowPlayingResponse uses pointers so "nothing played yet" encodes as null, like PHP.
type nowPlayingResponse struct {
	Track       *radio.Track `json:"track"`
	StartedAt   *string      `json:"started_at"`
	GeneratedAt string       `json:"generated_at"`
}

func (a *api) nowPlaying(w http.ResponseWriter, r *http.Request) {
	play, err := a.repo.NowPlaying(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	resp := nowPlayingResponse{GeneratedAt: formatTime(a.now())}
	if play != nil {
		track := play.Track
		track.Tags = a.normalizeTags(track.Tags)
		startedAt := formatTime(play.PlayedAt)
		resp.Track = &track
		resp.StartedAt = &startedAt
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// formatTime matches PHP's gmdate('Y-m-d\TH:i:s\Z'): UTC, whole seconds, literal Z. RFC3339 on a
// UTC time prints "Z" and, having no fractional layout, truncates the nanoseconds.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// normalizeTags guarantees an empty tag list is [] and never null.
//
// With injectBugs it reproduces, on purpose, the classic PHP-to-Go migration bug: PHP's
// json_encode(array()) is [], but a nil Go slice encodes as null. Clients that do
// `tags.length` break on null. It exists so shadow mode has a real divergence to detect.
func (a *api) normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		if a.injectBugs {
			return nil
		}
		return []string{}
	}
	return tags
}

func (a *api) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("api request failed",
		"err", err,
		"path", r.URL.Path,
		"request_id", httpx.RequestIDFrom(r.Context()),
	)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}
