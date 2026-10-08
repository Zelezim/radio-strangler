// Package admin is the control plane of the migration: it changes route modes at runtime and
// exposes the shadow evidence needed to decide each step.
package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Zelezim/radio-strangler/internal/httpx"
	"github.com/Zelezim/radio-strangler/internal/routing"
	"github.com/Zelezim/radio-strangler/internal/shadow"
)

const (
	maxBodyBytes      = 64 << 10
	defaultLimit      = 50
	maxLimit          = 500
	statsWindow       = 24 * time.Hour
	minStrongTokenLen = 16
)

// Store is the persistence the control plane needs.
type Store interface {
	ListRules(ctx context.Context) ([]routing.Rule, error)
	UpsertRule(ctx context.Context, r routing.Rule) (routing.Rule, string, error)
	ListComparisons(ctx context.Context, f shadow.Filter) ([]shadow.Result, error)
	ShadowStats(ctx context.Context, window time.Duration) ([]shadow.RouteStats, error)
}

// Reloader refreshes the in-memory routing table without blocking.
type Reloader interface {
	Trigger()
}

type admin struct {
	store    Store
	reloader Reloader
	token    []byte
	log      *slog.Logger
}

// New returns the /admin/ handler. An empty token disables the control plane entirely.
func New(store Store, reloader Reloader, token string, log *slog.Logger) http.Handler {
	if token == "" {
		log.Warn("ADMIN_TOKEN is empty: admin API disabled")
		return http.HandlerFunc(notFound)
	}
	if token == "change-me" || len(token) < minStrongTokenLen {
		log.Warn("ADMIN_TOKEN looks weak or is the example value; use a long random token outside local demos")
	}

	a := &admin{store: store, reloader: reloader, token: []byte(token), log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/routes", a.listRoutes)
	// {route...} captures the rest of the path, slashes included: PUT /admin/routes/api/tracks
	// targets the rule "/api/tracks".
	mux.HandleFunc("PUT /admin/routes/{route...}", a.putRoute)
	mux.HandleFunc("GET /admin/comparisons", a.listComparisons)
	mux.HandleFunc("GET /admin/stats", a.stats)
	mux.HandleFunc("/admin/", notFound)
	return a.requireToken(mux)
}

// requireToken guards every admin path, unknown ones included, so an anonymous caller cannot
// even map the API. The repository is public: the token is the only secret, and nothing here
// relies on the endpoints being hard to find.
func (a *admin) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearerToken(r.Header.Get("Authorization"))
		// Constant-time comparison: a byte-by-byte early exit would leak how much of a guess
		// was right through response timing.
		if !ok || subtle.ConstantTimeCompare([]byte(got), a.token) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
			httpx.Error(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the credentials of an "Authorization: Bearer <token>" header. The scheme
// name is case-insensitive per RFC 9110.
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return header[len(prefix):], true
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	httpx.Error(w, http.StatusNotFound, "not found")
}

type dataResponse[T any] struct {
	Data T `json:"data"`
}

func (a *admin) listRoutes(w http.ResponseWriter, r *http.Request) {
	rules, err := a.store.ListRules(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if rules == nil {
		rules = []routing.Rule{}
	}
	httpx.WriteJSON(w, http.StatusOK, dataResponse[[]routing.Rule]{Data: rules})
}

// ruleUpdate is the PUT body. Pointers distinguish "omitted" from zero values.
type ruleUpdate struct {
	Mode          routing.Mode `json:"mode"`
	CanaryPercent *int         `json:"canary_percent"`
	// Omitted keeps the current list (see store.UpsertRule); [] clears it.
	IgnoreFields []string `json:"ignore_fields"`
}

func (a *admin) putRoute(w http.ResponseWriter, r *http.Request) {
	var body ruleUpdate
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	// Typos like "canary_percen" must fail loudly, not silently fall back to 0%.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body: unexpected data after object")
		return
	}

	rule := routing.Rule{
		Route:        "/" + r.PathValue("route"),
		Mode:         body.Mode,
		IgnoreFields: body.IgnoreFields,
	}
	if body.CanaryPercent != nil {
		rule.CanaryPercent = *body.CanaryPercent
	}
	// "go" means all traffic on Go; storing 100 keeps the row self-describing and makes a later
	// switch back to canary start from an explicit value instead of a stale one.
	if rule.Mode == routing.ModeGo {
		rule.CanaryPercent = 100
	}
	if err := rule.Validate(); err != nil {
		httpx.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	saved, previous, err := a.store.UpsertRule(r.Context(), rule)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	// Apply now instead of waiting for the next periodic refresh.
	a.reloader.Trigger()

	a.log.Info("route rule updated",
		"route", saved.Route,
		"from", previous,
		"to", saved.Mode,
		"canary_percent", saved.CanaryPercent,
		"ignore_fields", saved.IgnoreFields,
		"request_id", httpx.RequestIDFrom(r.Context()),
	)
	httpx.WriteJSON(w, http.StatusOK, dataResponse[routing.Rule]{Data: saved})
}

func (a *admin) listComparisons(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := shadow.Filter{Route: q.Get("route"), Limit: defaultLimit}

	if v := q.Get("mismatches"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "mismatches must be true or false")
			return
		}
		f.MismatchesOnly = b
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			httpx.Error(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		f.Limit = min(n, maxLimit)
	}

	results, err := a.store.ListComparisons(r.Context(), f)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if results == nil {
		results = []shadow.Result{}
	}
	httpx.WriteJSON(w, http.StatusOK, dataResponse[[]shadow.Result]{Data: results})
}

type statsResponse struct {
	Window string              `json:"window"`
	Data   []shadow.RouteStats `json:"data"`
}

func (a *admin) stats(w http.ResponseWriter, r *http.Request) {
	stats, err := a.store.ShadowStats(r.Context(), statsWindow)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if stats == nil {
		stats = []shadow.RouteStats{}
	}
	httpx.WriteJSON(w, http.StatusOK, statsResponse{Window: statsWindow.String(), Data: stats})
}

func (a *admin) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("admin request failed",
		"err", err,
		"path", r.URL.Path,
		"request_id", httpx.RequestIDFrom(r.Context()),
	)
	httpx.Error(w, http.StatusInternalServerError, fmt.Sprintf("internal error (request %s)", httpx.RequestIDFrom(r.Context())))
}
