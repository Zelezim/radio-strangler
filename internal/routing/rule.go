// Package routing decides, per request, which backend serves a route during the migration.
package routing

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"
)

// Mode is the migration stage of a route.
type Mode string

const (
	ModeLegacy Mode = "legacy" // PHP serves.
	ModeShadow Mode = "shadow" // PHP serves; Go is called in the background and compared.
	ModeCanary Mode = "canary" // A stable percentage of clients is served by Go.
	ModeGo     Mode = "go"     // Go serves.
)

// Valid reports whether m is one of the known modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeLegacy, ModeShadow, ModeCanary, ModeGo:
		return true
	}
	return false
}

// Backend is the implementation that answers the client.
type Backend string

const (
	BackendLegacy Backend = "legacy"
	BackendGo     Backend = "go"
)

// Rule is the routing configuration of one route prefix.
type Rule struct {
	Route         string    `json:"route"`
	Mode          Mode      `json:"mode"`
	CanaryPercent int       `json:"canary_percent"`
	IgnoreFields  []string  `json:"ignore_fields"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// DefaultRule applies to any path without a rule. Unknown traffic always goes to the legacy
// system: a route is only ever moved to Go deliberately, never by omission.
var DefaultRule = Rule{Mode: ModeLegacy}

// Validate checks the invariants the matcher relies on.
func (r Rule) Validate() error {
	var errs []error
	if !strings.HasPrefix(r.Route, "/") {
		errs = append(errs, fmt.Errorf("route %q must start with /", r.Route))
	}
	// A trailing slash would silently break segment matching ("/api/" never matches "/api").
	if strings.HasSuffix(r.Route, "/") {
		errs = append(errs, fmt.Errorf("route %q must not end with /", r.Route))
	}
	if !r.Mode.Valid() {
		errs = append(errs, fmt.Errorf("route %q: unknown mode %q", r.Route, r.Mode))
	}
	if r.CanaryPercent < 0 || r.CanaryPercent > 100 {
		errs = append(errs, fmt.Errorf("route %q: canary_percent %d out of range 0..100", r.Route, r.CanaryPercent))
	}
	return errors.Join(errs...)
}

// Backend picks the implementation for a client. Shadow is served by legacy: the Go call it
// triggers happens off the request path.
func (r Rule) Backend(clientKey string) Backend {
	switch r.Mode {
	case ModeGo:
		return BackendGo
	case ModeCanary:
		if Bucket(clientKey) < r.CanaryPercent {
			return BackendGo
		}
	}
	return BackendLegacy
}

// Bucket maps a client key to 0..99. It is a pure hash rather than a random draw so the same
// client keeps landing on the same backend across requests and replicas, without shared state,
// and raising the percentage only ever adds clients to Go, it never reshuffles existing ones.
func Bucket(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key)) // hash.Hash.Write never returns an error
	return int(h.Sum32() % 100)
}
