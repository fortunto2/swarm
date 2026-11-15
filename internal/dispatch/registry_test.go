package dispatch

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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

func TestParseEtime(t *testing.T) {
	for in, want := range map[string]time.Duration{"00:05": 5 * time.Second, "01:02:03": time.Hour + 2*time.Minute + 3*time.Second,
		"2-00:00:10": 48*time.Hour + 10*time.Second} {
		if got, ok := parseEtime(in); !ok || got != want {
			t.Errorf("parseEtime(%q) = %v %v, want %v", in, got, ok, want)
		}
	}
	if _, ok := parseEtime("garbage"); ok {
		t.Error("garbage parsed")
	}
}

func TestLimitsReadingPausesAProvider(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	f := filepath.Join(t.TempDir(), "limits.json")
	write := func(at int64, used float64, resets int64) {
		_ = os.WriteFile(f, []byte(fmt.Sprintf(`{"at":%d,"limits":{"five_hour":{"used_percentage":%v,"resets_at":%d},"seven_day":{"used_percentage":10,"resets_at":%d}}}`,
			at, used, resets, now.Add(48*time.Hour).Unix())), 0o600)
	}
	p := config.Provider{LimitsFile: f, PauseAbove: 80}
	write(now.Unix(), 85, now.Add(time.Hour).Unix())
	if until, why := limitsReading(p, now); until != now.Add(time.Hour).Unix() || why == "" {
		t.Fatalf("85%% must pause until the reset: %d %q", until, why)
	}
	write(now.Unix(), 50, now.Add(time.Hour).Unix())
	if until, _ := limitsReading(p, now); until != 0 {
		t.Fatal("50% must not pause")
	}
	write(now.Add(-48*time.Hour).Unix(), 99, now.Add(time.Hour).Unix())
	if until, _ := limitsReading(p, now); until != 0 {
		t.Fatal("a reading two days old is unknown, not a reason to stop")
	}
	write(now.Unix(), 99, now.Add(-time.Minute).Unix())
	if until, _ := limitsReading(p, now); until != 0 {
		t.Fatal("a window already reset does not pause")
	}
}
