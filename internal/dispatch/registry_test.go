package dispatch

import (
	"slices"
	"testing"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/provider"
)

func TestEligibleOrderRolesAndState(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.Provider{
		"pricey":   {Priority: 2, Slots: 1},
		"cheap":    {Priority: 1, Slots: 1},
		"also-1":   {Priority: 1, Slots: 1},
		"reviewer": {Priority: 0, Slots: 1, Roles: []string{"reviewer"}},
		"a-only":   {Priority: 0, Slots: 1, Tiers: []string{"A"}},
		"cooling":  {Priority: 0, Slots: 1},
		"off":      {Priority: 0, Slots: 1},
	}}
	d := &Daemon{Cfg: cfg, Node: "local", adapters: map[string]provider.Adapter{}}
	for id := range cfg.Providers {
		d.adapters[id] = nil
	}
	d.reg = registry{states: map[string]db.ProviderState{"cooling": {State: "cooling"}, "off": {State: "disabled"}}}
	got := d.eligible("B")
	want := []string{"also-1", "cheap", "pricey"} // priority, then name; no reviewer, no A-only, no cooling/disabled
	if !slices.Equal(got, want) {
		t.Fatalf("eligible(B) = %v, want %v", got, want)
	}
	if id, ok := d.pickProvider("B", map[string]int{"also-1": 1}); !ok || id != "cheap" {
		t.Fatalf("a full provider is skipped for the next best: got %q", id)
	}
	if got := d.eligible("A"); got[0] != "a-only" {
		t.Fatalf("eligible(A) = %v", got)
	}
}

func TestRunningSeesTheLock(t *testing.T) {
	dir := t.TempDir()
	if Running(dir) {
		t.Fatal("no daemon yet")
	}
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !Running(dir) {
		t.Fatal("a held lock reads as no daemon")
	}
	release()
	if Running(dir) {
		t.Fatal("a released lock still reads as a daemon")
	}
}
