package main

import (
	"testing"
	"time"
)

// `swarm report --since 7d` is how the docs and both skills write a weekly look; a plain
// duration flag refused it ("unknown unit d").
func TestSinceTakesDays(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 168 * time.Hour, "24h": 24 * time.Hour, "90m": 90 * time.Minute, "0d": 0} {
		var d days
		if err := d.Set(in); err != nil || time.Duration(d) != want {
			t.Errorf("Set(%q) = %v, %v; want %v", in, time.Duration(d), err, want)
		}
	}
	for _, bad := range []string{"xd", "-1d", "7w"} {
		var d days
		if err := d.Set(bad); err == nil {
			t.Errorf("Set(%q) accepted", bad)
		}
	}
	if s := days(14 * 24 * time.Hour).String(); s != "14d" {
		t.Errorf("String() = %q, want 14d", s)
	}
}
