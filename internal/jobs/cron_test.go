package jobs

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04 Mon", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestScheduleMatches(t *testing.T) {
	cases := []struct {
		expr, when string
		want       bool
	}{
		{"0 14,21 * * 1-5", "2026-11-16 14:00 Mon", true},
		{"0 14,21 * * 1-5", "2026-11-16 21:00 Mon", true},
		{"0 14,21 * * 1-5", "2026-11-15 14:00 Sun", false}, // weekend
		{"0 14,21 * * 1-5", "2026-11-16 14:01 Mon", false},
		{"*/15 * * * *", "2026-11-16 03:45 Mon", true},
		{"*/15 * * * *", "2026-11-16 03:44 Mon", false},
		{"5/20 * * * *", "2026-11-16 03:25 Mon", true}, // 5, 25, 45
		{"5/20 * * * *", "2026-11-16 03:20 Mon", false},
		{"30 7 * * 0", "2026-11-15 07:30 Sun", true},
		{"30 7 * * 7", "2026-11-15 07:30 Sun", true}, // 7 is Sunday too
		{"0 0 1 * 1", "2026-12-01 00:00 Tue", true},  // day of month OR day of week, as cron
		{"0 0 1 * 1", "2026-11-16 00:00 Mon", true},
		{"0 0 1 * 1", "2026-11-17 00:00 Tue", false},
		{"0 9 * 11 *", "2026-11-16 09:00 Mon", true},
		{"0 0 */2 * 1", "2026-11-17 00:00 Tue", false}, // a starred step ANDs: odd days that are Mondays
		{"0 0 */2 * 1", "2026-11-23 00:00 Mon", true},
		{"0 0 */2 * 1", "2026-11-16 00:00 Mon", false}, // Monday, but the 16th is even
		{"0 9 * 12 *", "2026-11-16 09:00 Mon", false},
	}
	for _, c := range cases {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := s.Matches(at(c.when)); got != c.want {
			t.Errorf("%q at %s = %v, want %v", c.expr, c.when, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8",
		"*/0 * * * *", "5-3 * * * *", "a * * * *", "* * * * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPrevAndNext(t *testing.T) {
	s, _ := Parse("0 14,21 * * 1-5")
	now := at("2026-11-16 15:30 Mon")
	if p := s.Prev(now); !p.Equal(at("2026-11-16 14:00 Mon")) {
		t.Fatalf("prev = %v", p)
	}
	if n := s.Next(now); !n.Equal(at("2026-11-16 21:00 Mon")) {
		t.Fatalf("next = %v", n)
	}
	if p := s.Prev(at("2026-11-16 14:00 Mon")); !p.Equal(at("2026-11-16 14:00 Mon")) {
		t.Fatalf("prev at a scheduled minute = %v, want that minute", p)
	}
	if n := s.Next(at("2026-11-13 21:00 Fri")); !n.Equal(at("2026-11-16 14:00 Mon")) {
		t.Fatalf("next over a weekend = %v", n)
	}
}

// Any text either fails to parse or gives a schedule whose Prev and Next agree with Matches:
// a config typo must never panic the daemon at load, and a schedule must never skip itself.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"0 14,21 * * 1-5", "*/15 * * * *", "5/20 * * * *", "0 0 */2 * 1", "* * * * 7", "1-5/2 0 1,15 * *"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		s, err := Parse(expr)
		if err != nil {
			return
		}
		now := time.Date(2026, 11, 16, 12, 34, 0, 0, time.UTC)
		if p := s.Prev(now); !p.IsZero() && (!s.Matches(p) || p.After(now)) {
			t.Fatalf("%q: Prev(%v) = %v does not match or is after now", expr, now, p)
		}
		if n := s.Next(now); !n.IsZero() && (!s.Matches(n) || !n.After(now)) {
			t.Fatalf("%q: Next(%v) = %v does not match or is not after now", expr, now, n)
		}
	})
}
