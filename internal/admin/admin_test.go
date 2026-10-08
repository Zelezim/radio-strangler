package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zelezim/radio-strangler/internal/routing"
	"github.com/Zelezim/radio-strangler/internal/shadow"
)

const token = "s3cret-token-for-tests"

type fakeStore struct {
	rules      []routing.Rule
	upserted   []routing.Rule
	filter     shadow.Filter
	window     time.Duration
	upsertErr  error
	comparison []shadow.Result
}

func (f *fakeStore) ListRules(context.Context) ([]routing.Rule, error) { return f.rules, nil }

func (f *fakeStore) UpsertRule(_ context.Context, r routing.Rule) (routing.Rule, string, error) {
	if f.upsertErr != nil {
		return routing.Rule{}, "", f.upsertErr
	}
	f.upserted = append(f.upserted, r)
	if r.IgnoreFields == nil {
		r.IgnoreFields = []string{}
	}
	return r, "legacy", nil
}

func (f *fakeStore) ListComparisons(_ context.Context, flt shadow.Filter) ([]shadow.Result, error) {
	f.filter = flt
	return f.comparison, nil
}

func (f *fakeStore) ShadowStats(_ context.Context, window time.Duration) ([]shadow.RouteStats, error) {
	f.window = window
	return []shadow.RouteStats{{Route: "/api/tracks", Total: 4, Matched: 3, MatchRate: 0.75}}, nil
}

type fakeReloader struct{ n atomic.Int32 }

func (f *fakeReloader) Trigger() { f.n.Add(1) }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)) }

type env struct {
	store    *fakeStore
	reloader *fakeReloader
	handler  http.Handler
}

func newEnv(tok string) *env {
	e := &env{
		store:    &fakeStore{rules: []routing.Rule{{Route: "/api/tracks", Mode: routing.ModeShadow, IgnoreFields: []string{}}}},
		reloader: &fakeReloader{},
	}
	e.handler = New(e.store, e.reloader, tok, quiet())
	return e
}

func (e *env) do(method, path, body, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *env) authed(method, path, body string) *httptest.ResponseRecorder {
	return e.do(method, path, body, "Bearer "+token)
}

func TestDisabledWithoutToken(t *testing.T) {
	e := newEnv("")
	for _, auth := range []string{"", "Bearer ", "Bearer anything"} {
		if rec := e.do(http.MethodGet, "/admin/routes", "", auth); rec.Code != http.StatusNotFound {
			t.Errorf("auth %q: status %d, want 404", auth, rec.Code)
		}
	}
}

func TestRequiresBearerToken(t *testing.T) {
	e := newEnv(token)
	for _, auth := range []string{"", token, "Basic " + token, "Bearer wrong", "Bearer " + token + "x", "Bearer  " + token, "Bearer"} {
		rec := e.do(http.MethodGet, "/admin/routes", "", auth)
		if rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != `{"error":"unauthorized"}` {
			t.Errorf("auth %q: got %d %s", auth, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do(http.MethodGet, "/admin/routes", "", "bearer "+token); rec.Code != http.StatusOK {
		t.Errorf("lowercase scheme: %d, want 200 (scheme is case-insensitive)", rec.Code)
	}
	// Unknown admin paths are hidden behind auth too.
	if rec := e.do(http.MethodGet, "/admin/secret-thing", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown path without token: %d, want 401", rec.Code)
	}
	if rec := e.authed(http.MethodGet, "/admin/secret-thing", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path with token: %d, want 404", rec.Code)
	}
}

func TestListRoutes(t *testing.T) {
	rec := newEnv(token).authed(http.MethodGet, "/admin/routes", "")
	var got struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Data) != 1 {
		t.Fatalf("body = %s", rec.Body.String())
	}
	for _, k := range []string{"route", "mode", "canary_percent", "ignore_fields", "updated_at"} {
		if _, ok := got.Data[0][k]; !ok {
			t.Errorf("missing field %q in %v", k, got.Data[0])
		}
	}
}

func TestPutRoute(t *testing.T) {
	tests := []struct {
		name        string
		path, body  string
		wantStatus  int
		wantRule    *routing.Rule
		wantTrigger bool
	}{
		{
			name: "canary", path: "/admin/routes/api/programs", body: `{"mode":"canary","canary_percent":50}`,
			wantStatus: 200, wantRule: &routing.Rule{Route: "/api/programs", Mode: routing.ModeCanary, CanaryPercent: 50}, wantTrigger: true,
		},
		{
			name: "go forces 100%", path: "/admin/routes/api/programs", body: `{"mode":"go","canary_percent":10}`,
			wantStatus: 200, wantRule: &routing.Rule{Route: "/api/programs", Mode: routing.ModeGo, CanaryPercent: 100}, wantTrigger: true,
		},
		{
			name: "nested route and ignore fields", path: "/admin/routes/api/v2/now-playing", body: `{"mode":"shadow","ignore_fields":["generated_at"]}`,
			wantStatus: 200, wantRule: &routing.Rule{Route: "/api/v2/now-playing", Mode: routing.ModeShadow, IgnoreFields: []string{"generated_at"}}, wantTrigger: true,
		},
		{name: "unknown mode", path: "/admin/routes/api/programs", body: `{"mode":"blue-green"}`, wantStatus: 422},
		{name: "missing mode", path: "/admin/routes/api/programs", body: `{}`, wantStatus: 422},
		{name: "percent out of range", path: "/admin/routes/api/programs", body: `{"mode":"canary","canary_percent":150}`, wantStatus: 422},
		{name: "trailing slash route", path: "/admin/routes/api/programs/", body: `{"mode":"go"}`, wantStatus: 422},
		{name: "unknown field", path: "/admin/routes/api/programs", body: `{"mode":"canary","canary_percen":50}`, wantStatus: 400},
		{name: "malformed JSON", path: "/admin/routes/api/programs", body: `{"mode":`, wantStatus: 400},
		{name: "trailing data", path: "/admin/routes/api/programs", body: `{"mode":"go"} {}`, wantStatus: 400},
		{name: "body too large", path: "/admin/routes/api/programs", body: `{"mode":"go","ignore_fields":["` + strings.Repeat("x", 70<<10) + `"]}`, wantStatus: 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(token)
			rec := e.authed(http.MethodPut, tt.path, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if triggered := e.reloader.n.Load() > 0; triggered != tt.wantTrigger {
				t.Errorf("reload triggered = %v, want %v", triggered, tt.wantTrigger)
			}
			if tt.wantRule == nil {
				if len(e.store.upserted) != 0 {
					t.Errorf("invalid request reached the store: %+v", e.store.upserted)
				}
				return
			}
			got := e.store.upserted[0]
			if got.Route != tt.wantRule.Route || got.Mode != tt.wantRule.Mode || got.CanaryPercent != tt.wantRule.CanaryPercent ||
				strings.Join(got.IgnoreFields, ",") != strings.Join(tt.wantRule.IgnoreFields, ",") {
				t.Errorf("upserted %+v, want %+v", got, *tt.wantRule)
			}
		})
	}
}

func TestPutOmittedIgnoreFieldsArePassedAsNil(t *testing.T) {
	e := newEnv(token)
	e.authed(http.MethodPut, "/admin/routes/api/now-playing", `{"mode":"go"}`)
	if got := e.store.upserted[0].IgnoreFields; got != nil {
		t.Errorf("IgnoreFields = %#v, want nil (keep current)", got)
	}
}

func TestPutStoreFailure(t *testing.T) {
	e := newEnv(token)
	e.store.upsertErr = errors.New("db down")
	if rec := e.authed(http.MethodPut, "/admin/routes/api/programs", `{"mode":"go"}`); rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", rec.Code)
	}
	if e.reloader.n.Load() != 0 {
		t.Error("reload must not be triggered when the write failed")
	}
}

func TestListComparisonsFilters(t *testing.T) {
	tests := []struct {
		query      string
		wantStatus int
		want       shadow.Filter
	}{
		{"", 200, shadow.Filter{Limit: 50}},
		{"?route=/api/tracks&mismatches=true&limit=10", 200, shadow.Filter{Route: "/api/tracks", MismatchesOnly: true, Limit: 10}},
		{"?limit=100000", 200, shadow.Filter{Limit: 500}},
		{"?limit=0", 400, shadow.Filter{}},
		{"?limit=abc", 400, shadow.Filter{}},
		{"?mismatches=maybe", 400, shadow.Filter{}},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			e := newEnv(token)
			rec := e.authed(http.MethodGet, "/admin/comparisons"+tt.query, "")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == 200 && e.store.filter != tt.want {
				t.Errorf("filter %+v, want %+v", e.store.filter, tt.want)
			}
			if tt.wantStatus == 200 && strings.TrimSpace(rec.Body.String()) != `{"data":[]}` {
				t.Errorf("empty result must be [], got %s", rec.Body.String())
			}
		})
	}
}

func TestStatsUses24hWindow(t *testing.T) {
	e := newEnv(token)
	rec := e.authed(http.MethodGet, "/admin/stats", "")
	if rec.Code != 200 || e.store.window != 24*time.Hour {
		t.Fatalf("status %d window %s", rec.Code, e.store.window)
	}
	if !strings.Contains(rec.Body.String(), `"window":"24h0m0s"`) || !strings.Contains(rec.Body.String(), `"match_rate":0.75`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}
