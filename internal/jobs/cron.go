// Package jobs: scheduled work in the daemon (roadmap 8). A job is a shell command on a cron
// schedule, run by the coordinator, recorded with its exit and the tail of its output, and
// told to the operator when it fails. This file is the schedule: five cron fields, UTC.
package jobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule: minute hour day-of-month month day-of-week, each a set of allowed values.
type Schedule struct {
	min, hour, dom, mon, dow [64]bool
	domStar, dowStar         bool
}

var fields = []struct {
	name     string
	lo, hi   int
	aliasDow bool
}{{"minute", 0, 59, false}, {"hour", 0, 23, false}, {"day of month", 1, 31, false}, {"month", 1, 12, false}, {"day of week", 0, 7, true}}

// Parse reads "m h dom mon dow": *, a value, a-b ranges, lists with commas, /n steps.
// Day of week 0 and 7 are Sunday. Like cron, when both day fields are restricted a day
// matches either of them.
func Parse(expr string) (Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Schedule{}, fmt.Errorf("cron %q: want 5 fields (minute hour day-of-month month day-of-week), got %d", expr, len(parts))
	}
	var s Schedule
	sets := []*[64]bool{&s.min, &s.hour, &s.dom, &s.mon, &s.dow}
	for i, p := range parts {
		f := fields[i]
		for _, item := range strings.Split(p, ",") {
			lo, hi, step := f.lo, f.hi, 1
			rng := item
			if a, b, ok := strings.Cut(item, "/"); ok {
				n, err := strconv.Atoi(b)
				if err != nil || n < 1 {
					return Schedule{}, fmt.Errorf("cron %q: %s step %q", expr, f.name, b)
				}
				step, rng = n, a
			}
			switch {
			case rng == "*":
			case strings.Contains(rng, "-"):
				a, b, _ := strings.Cut(rng, "-")
				x, err1 := strconv.Atoi(a)
				y, err2 := strconv.Atoi(b)
				if err1 != nil || err2 != nil || x > y {
					return Schedule{}, fmt.Errorf("cron %q: %s range %q", expr, f.name, rng)
				}
				lo, hi = x, y
			default:
				x, err := strconv.Atoi(rng)
				if err != nil {
					return Schedule{}, fmt.Errorf("cron %q: %s value %q", expr, f.name, rng)
				}
				lo, hi = x, x
				if strings.Contains(item, "/") {
					hi = f.hi // "5/15": from 5 on, every 15
				}
			}
			if lo < f.lo || hi > f.hi {
				return Schedule{}, fmt.Errorf("cron %q: %s %d-%d outside %d-%d", expr, f.name, lo, hi, f.lo, f.hi)
			}
			for v := lo; v <= hi; v += step {
				sets[i][v] = true
			}
		}
	}
	if s.dow[7] {
		s.dow[0] = true
	}
	s.domStar, s.dowStar = parts[2] == "*", parts[4] == "*"
	return s, nil
}

// Matches: the minute of t (UTC) is one the schedule names.
func (s Schedule) Matches(t time.Time) bool {
	t = t.UTC()
	if !s.min[t.Minute()] || !s.hour[t.Hour()] || !s.mon[int(t.Month())] {
		return false
	}
	dom, dow := s.dom[t.Day()], s.dow[int(t.Weekday())]
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	}
	return dom || dow
}

// Prev: the latest scheduled minute at or before t; zero if none within a year and a day.
func (s Schedule) Prev(t time.Time) time.Time {
	m := t.UTC().Truncate(time.Minute)
	for i := 0; i < 366*24*60+1440; i++ {
		if s.Matches(m) {
			return m
		}
		m = m.Add(-time.Minute)
	}
	return time.Time{}
}

// Next: the first scheduled minute after t; zero if none within a year and a day.
func (s Schedule) Next(t time.Time) time.Time {
	m := t.UTC().Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60+1440; i++ {
		if s.Matches(m) {
			return m
		}
		m = m.Add(time.Minute)
	}
	return time.Time{}
}
