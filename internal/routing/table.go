package routing

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// Table is the in-memory rule set. Reads are lock-free: every request does a Match, while
// writes only happen on reload, so the whole slice is swapped atomically instead of locked.
type Table struct {
	rules atomic.Pointer[[]Rule]
}

// NewTable returns an empty table, which sends everything to legacy.
func NewTable() *Table {
	t := &Table{}
	t.Replace(nil)
	return t
}

// Replace installs a new rule set.
func (t *Table) Replace(rules []Rule) {
	sorted := slices.Clone(rules)
	// Longest route first, so the first hit in Match is the most specific rule.
	slices.SortStableFunc(sorted, func(a, b Rule) int {
		if c := cmp.Compare(len(b.Route), len(a.Route)); c != 0 {
			return c
		}
		return cmp.Compare(a.Route, b.Route)
	})
	t.rules.Store(&sorted)
}

// Rules returns a snapshot of the current rules, longest route first.
func (t *Table) Rules() []Rule {
	return slices.Clone(*t.rules.Load())
}

// Match returns the most specific rule for path, or DefaultRule.
func (t *Table) Match(path string) Rule {
	for _, r := range *t.rules.Load() {
		if matchesPrefix(r.Route, path) {
			return r
		}
	}
	return DefaultRule
}

// matchesPrefix only matches on segment boundaries: /api/tracks covers /api/tracks/42 but not
// /api/tracksearch, which is a different resource and must not be migrated by accident.
func matchesPrefix(route, path string) bool {
	if !strings.HasPrefix(path, route) {
		return false
	}
	return len(path) == len(route) || path[len(route)] == '/'
}

// Source provides the persisted rules.
type Source interface {
	ListRules(ctx context.Context) ([]Rule, error)
}

// Loader keeps a Table in sync with its Source.
type Loader struct {
	table   *Table
	src     Source
	every   time.Duration
	log     *slog.Logger
	trigger chan struct{}
}

// NewLoader creates a loader that refreshes table from src every interval.
func NewLoader(table *Table, src Source, every time.Duration, log *slog.Logger) *Loader {
	return &Loader{
		table: table,
		src:   src,
		every: every,
		log:   log,
		// Capacity 1 coalesces bursts of triggers into a single pending reload.
		trigger: make(chan struct{}, 1),
	}
}

// Load fetches the rules once and installs the valid ones.
func (l *Loader) Load(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rules, err := l.src.ListRules(ctx)
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}
	valid := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if err := r.Validate(); err != nil {
			// Dropping one bad row is safer than rejecting the whole set: the route falls back
			// to legacy, which is always a correct (if unmigrated) answer.
			l.log.Warn("ignoring invalid route rule", "err", err)
			continue
		}
		valid = append(valid, r)
	}
	l.table.Replace(valid)
	l.log.Debug("route rules loaded", "count", len(valid))
	return nil
}

// Trigger asks Run for an immediate reload (e.g. after an admin change). It never blocks.
func (l *Loader) Trigger() {
	select {
	case l.trigger <- struct{}{}:
	default:
	}
}

// Run reloads periodically and on Trigger until ctx is done.
func (l *Loader) Run(ctx context.Context) {
	ticker := time.NewTicker(l.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-l.trigger:
		}
		if err := l.Load(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			// The table still holds the last good rules; a database blip must not change routing.
			l.log.Warn("route rules reload failed, keeping last good rules", "err", err)
		}
	}
}
