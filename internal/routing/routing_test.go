package routing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
)

func TestMatch(t *testing.T) {
	table := NewTable()
	table.Replace([]Rule{
		{Route: "/api", Mode: ModeShadow},
		{Route: "/api/tracks", Mode: ModeGo},
		{Route: "/api/now-playing", Mode: ModeCanary, CanaryPercent: 50},
	})

	tests := []struct {
		path      string
		wantRoute string
		wantMode  Mode
	}{
		{"/api/tracks", "/api/tracks", ModeGo},
		{"/api/tracks/42", "/api/tracks", ModeGo},            // longest prefix wins over /api
		{"/api/tracksearch", "/api", ModeShadow},             // not a segment boundary for /api/tracks
		{"/api/now-playing", "/api/now-playing", ModeCanary}, // exact
		{"/api/programs", "/api", ModeShadow},                // shorter prefix
		{"/api", "/api", ModeShadow},                         // exact on the short rule
		{"/apix", "", ModeLegacy},                            // not a segment boundary for /api
		{"/healthz", "", ModeLegacy},                         // no rule at all
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := table.Match(tt.path)
			if got.Route != tt.wantRoute || got.Mode != tt.wantMode {
				t.Errorf("Match(%q) = %q/%s, want %q/%s", tt.path, got.Route, got.Mode, tt.wantRoute, tt.wantMode)
			}
		})
	}
}

func TestEmptyTableDefaultsToLegacy(t *testing.T) {
	table := NewTable()
	for _, p := range []string{"/", "/api/tracks", "/anything/else"} {
		r := table.Match(p)
		if r.Mode != ModeLegacy || r.Backend("client") != BackendLegacy {
			t.Errorf("Match(%q) = %+v, want legacy", p, r)
		}
	}
}

func TestBackendByMode(t *testing.T) {
	tests := []struct {
		rule Rule
		want Backend
	}{
		{Rule{Mode: ModeLegacy}, BackendLegacy},
		{Rule{Mode: ModeShadow}, BackendLegacy},
		{Rule{Mode: ModeGo}, BackendGo},
		{Rule{Mode: ModeCanary, CanaryPercent: 0}, BackendLegacy},
		{Rule{Mode: ModeCanary, CanaryPercent: 100}, BackendGo},
	}
	for _, tt := range tests {
		for i := 0; i < 50; i++ {
			key := fmt.Sprintf("client-%d", i)
			if got := tt.rule.Backend(key); got != tt.want {
				t.Fatalf("%s/%d%% for %s = %s, want %s", tt.rule.Mode, tt.rule.CanaryPercent, key, got, tt.want)
			}
		}
	}
}

func TestCanaryIsStablePerClient(t *testing.T) {
	rule := Rule{Mode: ModeCanary, CanaryPercent: 20}
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("client-%d", i)
		first := rule.Backend(key)
		for j := 0; j < 10; j++ {
			if got := rule.Backend(key); got != first {
				t.Fatalf("client %s flipped from %s to %s", key, first, got)
			}
		}
	}
}

func TestCanaryDistribution(t *testing.T) {
	rule := Rule{Mode: ModeCanary, CanaryPercent: 20}
	const n = 10_000
	onGo := 0
	for i := 0; i < n; i++ {
		if rule.Backend(fmt.Sprintf("10.0.%d.%d", i/256, i%256)) == BackendGo {
			onGo++
		}
	}
	pct := float64(onGo) * 100 / n
	if pct < 18 || pct > 22 {
		t.Errorf("%.2f%% of clients on go, want ~20%%", pct)
	}
}

func TestCanaryRampOnlyAddsClients(t *testing.T) {
	low := Rule{Mode: ModeCanary, CanaryPercent: 10}
	high := Rule{Mode: ModeCanary, CanaryPercent: 30}
	for i := 0; i < 2000; i++ {
		key := fmt.Sprintf("client-%d", i)
		if low.Backend(key) == BackendGo && high.Backend(key) != BackendGo {
			t.Fatalf("client %s left go when the canary grew", key)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		rule    Rule
		wantErr bool
	}{
		{"valid", Rule{Route: "/api/tracks", Mode: ModeShadow}, false},
		{"valid canary bounds", Rule{Route: "/api", Mode: ModeCanary, CanaryPercent: 100}, false},
		{"missing leading slash", Rule{Route: "api/tracks", Mode: ModeLegacy}, true},
		{"trailing slash", Rule{Route: "/api/tracks/", Mode: ModeLegacy}, true},
		{"root only", Rule{Route: "/", Mode: ModeLegacy}, true},
		{"unknown mode", Rule{Route: "/api", Mode: "blue-green"}, true},
		{"percent below 0", Rule{Route: "/api", Mode: ModeCanary, CanaryPercent: -1}, true},
		{"percent above 100", Rule{Route: "/api", Mode: ModeCanary, CanaryPercent: 101}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.rule.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

type fakeSource struct {
	rules []Rule
	err   error
}

func (f *fakeSource) ListRules(context.Context) ([]Rule, error) { return f.rules, f.err }

func TestLoaderKeepsLastGoodRulesOnError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
	table := NewTable()
	src := &fakeSource{rules: []Rule{
		{Route: "/api/tracks", Mode: ModeGo},
		{Route: "/bad/", Mode: ModeGo}, // invalid: dropped, falls back to legacy
	}}
	loader := NewLoader(table, src, 0, log)

	if err := loader.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := table.Match("/api/tracks").Mode; got != ModeGo {
		t.Fatalf("mode = %s, want go", got)
	}
	if got := table.Match("/bad").Mode; got != ModeLegacy {
		t.Errorf("invalid rule was installed: mode = %s", got)
	}

	src.err = errors.New("database down")
	if err := loader.Load(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if got := table.Match("/api/tracks").Mode; got != ModeGo {
		t.Errorf("rules lost after failed reload: mode = %s", got)
	}
}

func TestTriggerNeverBlocks(t *testing.T) {
	loader := NewLoader(NewTable(), &fakeSource{}, 0, slog.Default())
	for i := 0; i < 10; i++ {
		loader.Trigger() // nobody is reading; must not deadlock
	}
}
