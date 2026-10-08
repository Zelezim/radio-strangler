package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Zelezim/radio-strangler/internal/radio"
)

type fakeRepo struct {
	programs []radio.Program
	tracks   []radio.Track
	play     *radio.Play
	err      error
}

func (f *fakeRepo) ListPrograms(context.Context) ([]radio.Program, error) { return f.programs, f.err }
func (f *fakeRepo) ListTracks(context.Context) ([]radio.Track, error)     { return f.tracks, f.err }
func (f *fakeRepo) NowPlaying(context.Context) (*radio.Play, error)       { return f.play, f.err }

func ptr(s string) *string { return &s }

var fixedNow = time.Date(2026, 10, 8, 14, 0, 0, 999_999_999, time.UTC)

func serve(t *testing.T, repo Repo, injectBugs bool, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	log := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
	h := newAPI(repo, injectBugs, log, func() time.Time { return fixedNow })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func body(rec *httptest.ResponseRecorder) string {
	return strings.TrimSuffix(rec.Body.String(), "\n")
}

func sampleTracks() []radio.Track {
	return []radio.Track{
		{ID: 1, Title: "Southern Lights", Artist: "The Wattle Band", Album: ptr("Coastal Roads"), DurationSeconds: 245, Tags: []string{"indie", "australian"}},
		// nil and empty tags must both come out as [].
		{ID: 3, Title: "Station Ident #3", Artist: "Southern Cross FM", DurationSeconds: 12, Tags: nil},
		{ID: 5, Title: "Quiet Hour", Artist: "Nobody", DurationSeconds: 60, Tags: []string{}},
	}
}

func TestTracksExactJSON(t *testing.T) {
	rec := serve(t, &fakeRepo{tracks: sampleTracks()}, false, http.MethodGet, "/api/tracks")

	want := `{"data":[` +
		`{"id":1,"title":"Southern Lights","artist":"The Wattle Band","album":"Coastal Roads","duration_seconds":245,"tags":["indie","australian"]},` +
		`{"id":3,"title":"Station Ident #3","artist":"Southern Cross FM","album":null,"duration_seconds":12,"tags":[]},` +
		`{"id":5,"title":"Quiet Hour","artist":"Nobody","album":null,"duration_seconds":60,"tags":[]}]}`
	if rec.Code != http.StatusOK || body(rec) != want {
		t.Errorf("got %d\n%s\nwant\n%s", rec.Code, body(rec), want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestInjectedBugEmitsNullTags(t *testing.T) {
	rec := serve(t, &fakeRepo{tracks: sampleTracks()}, true, http.MethodGet, "/api/tracks")

	if got := strings.Count(body(rec), `"tags":null`); got != 2 {
		t.Errorf(`want 2 "tags":null, got %d in %s`, got, body(rec))
	}
	if !strings.Contains(body(rec), `"tags":["indie","australian"]`) {
		t.Errorf("non-empty tags must be untouched: %s", body(rec))
	}
}

func TestEmptyListsAreArrays(t *testing.T) {
	for _, path := range []string{"/api/programs", "/api/tracks"} {
		rec := serve(t, &fakeRepo{}, false, http.MethodGet, path)
		if body(rec) != `{"data":[]}` {
			t.Errorf("%s = %s, want {\"data\":[]}", path, body(rec))
		}
	}
}

func TestProgramsJSON(t *testing.T) {
	repo := &fakeRepo{programs: []radio.Program{
		{ID: 3, Name: "Outback Drive", Host: "Jack O'Brien", Weekday: 1, StartTime: "16:00", EndTime: "18:00"},
	}}
	rec := serve(t, repo, false, http.MethodGet, "/api/programs")

	want := `{"data":[{"id":3,"name":"Outback Drive","host":"Jack O'Brien","weekday":1,"start_time":"16:00","end_time":"18:00"}]}`
	if body(rec) != want {
		t.Errorf("got  %s\nwant %s", body(rec), want)
	}
}

func TestNowPlayingUsesUTCWholeSeconds(t *testing.T) {
	sydney := time.FixedZone("AEST", 10*60*60)
	repo := &fakeRepo{play: &radio.Play{
		Track:    radio.Track{ID: 3, Title: "Station Ident #3", Artist: "Southern Cross FM", DurationSeconds: 12},
		PlayedAt: time.Date(2026, 10, 7, 9, 30, 15, 123_456_789, sydney),
	}}
	rec := serve(t, repo, false, http.MethodGet, "/api/now-playing")

	want := `{"track":{"id":3,"title":"Station Ident #3","artist":"Southern Cross FM","album":null,"duration_seconds":12,"tags":[]},` +
		`"started_at":"2026-10-06T23:30:15Z","generated_at":"2026-10-08T14:00:00Z"}`
	if body(rec) != want {
		t.Errorf("got  %s\nwant %s", body(rec), want)
	}
}

func TestNowPlayingWithNothingPlayed(t *testing.T) {
	rec := serve(t, &fakeRepo{}, false, http.MethodGet, "/api/now-playing")

	want := `{"track":null,"started_at":null,"generated_at":"2026-10-08T14:00:00Z"}`
	if body(rec) != want {
		t.Errorf("got  %s\nwant %s", body(rec), want)
	}
}

func TestMethodNotAllowedContract(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := serve(t, &fakeRepo{}, false, method, "/api/tracks")
		if rec.Code != http.StatusMethodNotAllowed || body(rec) != `{"error":"method not allowed"}` {
			t.Errorf("%s: got %d %s", method, rec.Code, body(rec))
		}
		if allow := rec.Header().Get("Allow"); allow != "GET" {
			t.Errorf("%s: Allow = %q, want GET", method, allow)
		}
	}
	if rec := serve(t, &fakeRepo{}, false, http.MethodHead, "/api/tracks"); rec.Code != http.StatusOK {
		t.Errorf("HEAD: got %d, want 200", rec.Code)
	}
}

func TestNotFoundContract(t *testing.T) {
	tests := []struct{ method, path string }{
		{http.MethodGet, "/nope"},
		{http.MethodGet, "/api/tracks/42"},
		{http.MethodGet, "/api/tracksearch"},
		{http.MethodPost, "/nope"}, // unknown path wins over wrong method, as in PHP
	}
	for _, tt := range tests {
		rec := serve(t, &fakeRepo{}, false, tt.method, tt.path)
		if rec.Code != http.StatusNotFound || body(rec) != `{"error":"not found"}` {
			t.Errorf("%s %s: got %d %s", tt.method, tt.path, rec.Code, body(rec))
		}
	}
}

func TestRepoErrorIsGeneric500(t *testing.T) {
	repo := &fakeRepo{err: errors.New(`pq: password authentication failed for user "postgres"`)}
	for _, path := range []string{"/api/programs", "/api/tracks", "/api/now-playing"} {
		rec := serve(t, repo, false, http.MethodGet, path)
		if rec.Code != http.StatusInternalServerError || body(rec) != `{"error":"internal error"}` {
			t.Errorf("%s: got %d %s", path, rec.Code, body(rec))
		}
	}
}
