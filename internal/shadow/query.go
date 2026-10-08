package shadow

import "time"

// Filter selects stored comparisons.
type Filter struct {
	Route          string // "" means every route
	MismatchesOnly bool
	Limit          int
}

// RouteStats summarizes the comparisons of one route over a time window. It is the evidence
// used to decide whether a route can move from shadow to canary, and from canary to go.
type RouteStats struct {
	Route     string    `json:"route"`
	Total     int64     `json:"total"`
	Matched   int64     `json:"matched"`
	Errors    int64     `json:"errors"`
	MatchRate float64   `json:"match_rate"`
	LegacyP95 float64   `json:"legacy_p95_ms"`
	GoP95     float64   `json:"go_p95_ms"`
	LastSeen  time.Time `json:"last_seen"`
}
