package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
)

// The provider registry: declared in swarm.toml (kind, roles, priority, slots, tiers,
// budget), live in the providers table (ok, cooling until, disabled), spend from attempts.
// refreshRegistry runs once per schedule pass; everything else reads the snapshot.

type registry struct {
	states map[string]db.ProviderState
	spent  map[string]float64 // dollars this UTC day
}

func utcDay(now time.Time) (start, next time.Time) {
	start = now.UTC().Truncate(24 * time.Hour)
	return start, start.Add(24 * time.Hour)
}

func (d *Daemon) refreshRegistry(ctx context.Context) error {
	now := time.Now()
	dayStart, nextDay := utcDay(now)
	spent, err := d.DB.SpentSinceByProvider(ctx, dayStart.Unix())
	if err != nil {
		return err
	}
	states, err := d.DB.ProviderStates(ctx)
	if err != nil {
		return err
	}
	for id, st := range states {
		if st.State == "cooling" && st.CoolingUntil <= now.Unix() {
			_ = d.DB.WarmProvider(ctx, id)
			st.State, st.CoolingUntil = "ok", 0
			states[id] = st
			d.DB.Event(ctx, "swarmd", 0, "provider_back", map[string]string{"provider": id})
		}
	}
	for id, p := range d.Cfg.Providers {
		st := states[id]
		remote := d.Cfg.Remote(d.Cfg.NodeOf(id)) // its limits file is on that machine: the node reports it
		if until, why := limitsReading(p, now); !remote && until > 0 && st.State != "cooling" && st.State != "disabled" {
			if err := d.DB.CoolProvider(ctx, id, until, why); err != nil {
				return err
			}
			states[id] = db.ProviderState{State: "cooling", CoolingUntil: until, LastError: why}
			d.DB.Event(ctx, "swarmd", 0, "provider_cooling", map[string]string{"provider": id, "why": why})
			d.Log.Info("provider cooling", "provider", id, "why", why)
			continue
		}
		if p.BudgetUSDDay > 0 && spent[id] >= p.BudgetUSDDay && st.State != "cooling" && st.State != "disabled" {
			why := fmt.Sprintf("daily budget spent: $%.2f of $%.2f", spent[id], p.BudgetUSDDay)
			if err := d.DB.CoolProvider(ctx, id, nextDay.Unix(), why); err != nil {
				return err
			}
			states[id] = db.ProviderState{State: "cooling", CoolingUntil: nextDay.Unix(), LastError: why}
			d.DB.Event(ctx, "swarmd", 0, "provider_cooling", map[string]string{"provider": id, "why": why})
			d.Log.Warn("provider cooling", "provider", id, "why", why, "until", nextDay)
			d.notify(fmt.Sprintf("swarm %s: %s paused until %s UTC: %s", d.Cfg.Profile.Name, id, nextDay.Format("Jan 2 15:04"), why))
		}
	}
	d.reg = registry{states: states, spent: spent}
	return nil
}

// eligible: providers that may work a task of this tier now, best first: a worker role,
// the tier, logged in on this node, not disabled or cooling. Ordered by priority, then id.
// Slots are not considered here: a busy provider is still eligible, just not free.
func (d *Daemon) eligible(tier string) []string {
	var ids []string
	for id, p := range d.Cfg.Providers {
		if !p.Can("worker") || (len(p.Tiers) > 0 && !slices.Contains(p.Tiers, tier)) {
			continue
		}
		if _, here := d.adapters[id]; !here && !d.Cfg.Remote(d.Cfg.NodeOf(id)) {
			continue // a local provider whose CLI could not be set up, or was disabled
		}
		if st := d.reg.states[id]; st.State == "cooling" || st.State == "disabled" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		pi, pj := d.Cfg.Providers[ids[i]].Priority, d.Cfg.Providers[ids[j]].Priority
		if pi != pj {
			return pi < pj
		}
		return ids[i] < ids[j]
	})
	return ids
}

// pickProvider: the best eligible provider with a free slot.
func (d *Daemon) pickProvider(tier string, busy map[string]int, fits func(id string) bool) (string, bool) {
	for _, id := range d.eligible(tier) {
		if busy[id] < d.Cfg.Providers[id].Slots && (fits == nil || fits(id)) {
			return id, true
		}
	}
	return "", false
}

// ProviderView: one row of `swarm providers` and of swarm_status.
type ProviderView struct {
	ID         string   `json:"id"`
	Node       string   `json:"node"`
	Kind       string   `json:"kind"`
	Family     string   `json:"family"`
	Roles      []string `json:"roles"`
	Priority   int      `json:"priority"`
	Slots      int      `json:"slots"`
	Running    int      `json:"running"`
	Tiers      []string `json:"tiers,omitempty"`
	State      string   `json:"state"`
	Until      string   `json:"until,omitempty"`
	Why        string   `json:"why,omitempty"`
	SpentToday float64  `json:"spent_today_usd"`
	BudgetDay  float64  `json:"budget_day_usd,omitempty"`
}

// ProviderViews reads the registry without a daemon: the declared half from the config,
// the live half from the database.
func ProviderViews(ctx context.Context, cfg *config.Config, d *db.DB) ([]ProviderView, error) {
	states, err := d.ProviderStates(ctx)
	if err != nil {
		return nil, err
	}
	dayStart, _ := utcDay(time.Now())
	spent, err := d.SpentSinceByProvider(ctx, dayStart.Unix())
	if err != nil {
		return nil, err
	}
	running, err := d.RunningByProvider(ctx)
	if err != nil {
		return nil, err
	}
	var out []ProviderView
	for _, id := range slices.Sorted(maps.Keys(cfg.Providers)) {
		p := cfg.Providers[id]
		roles := p.Roles
		if len(roles) == 0 {
			roles = []string{"worker"}
		}
		st := states[id]
		v := ProviderView{ID: id, Node: cfg.NodeOf(id), Kind: p.Kind, Family: p.Family, Roles: roles, Priority: p.Priority,
			Slots: p.Slots, Running: running[id], Tiers: p.Tiers, State: st.State, Why: st.LastError,
			SpentToday: spent[id], BudgetDay: p.BudgetUSDDay}
		if v.State == "" {
			v.State = "ok"
		}
		if st.CoolingUntil > 0 {
			v.Until = time.Unix(st.CoolingUntil, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out, nil
}

// limitsReading: (reset time, why) when the provider's limits file shows a window at or over
// its threshold and not yet reset; (0, "") otherwise, including when there is no readable file
// or the reading is a day old (an unknown is not a reason to stop work).
func limitsReading(p config.Provider, now time.Time) (int64, string) {
	if p.LimitsFile == "" {
		return 0, ""
	}
	b, err := os.ReadFile(p.LimitsFile)
	if err != nil {
		return 0, ""
	}
	var r struct {
		At     int64 `json:"at"`
		Limits map[string]struct {
			Used   *float64 `json:"used_percentage"`
			Resets int64    `json:"resets_at"`
		} `json:"limits"`
	}
	if json.Unmarshal(b, &r) != nil || (r.At > 0 && now.Unix()-r.At > 86400) {
		return 0, ""
	}
	var until int64
	why := ""
	for win, w := range r.Limits {
		if w.Used == nil || w.Resets <= now.Unix() || *w.Used < p.PauseAbove {
			continue
		}
		if w.Resets > until {
			until, why = w.Resets, fmt.Sprintf("%s at %.0f%% (pause_above %.0f%%)", win, *w.Used, p.PauseAbove)
		}
	}
	return until, why
}

// LimitsReading: until when a provider's account is over its limit, by its limits file
// (0 when it is not). A node reads its own accounts' files and reports them.
func LimitsReading(p config.Provider, now time.Time) (int64, string) { return limitsReading(p, now) }
