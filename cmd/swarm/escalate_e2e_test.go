package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A tier-B task whose first two attempts fail the gate goes to a tier-A provider on the third
// (limits.escalate_after, default 2), and merges there.
func TestEscalationToTierAAfterTwoFailures(t *testing.T) {
	repo := testbed(t)
	dir := t.TempDir()
	weak := filepath.Join(dir, "weak.sh")
	strong := filepath.Join(dir, "strong.sh")
	bad := `printf 'package app\n\nfunc Two() int { return 3 }\n' > two.go
printf 'package app\n\nimport "testing"\n\nfunc TestTwo(t *testing.T) { if Two() != 2 { t.Fatal() } }\n' > two_test.go
git add -A && git commit -qm "T$SWARM_TASK_ID: add Two" --allow-empty
swarm board done "$(git rev-parse HEAD)"
`
	for path, body := range map[string]string{weak: bad, strong: goodWorker} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nset -e\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prof := profile(t, repo, "exit 1", 4)
	b, _ := os.ReadFile(filepath.Join(prof, "swarm.toml"))
	s := string(b)
	i := strings.Index(s, "[providers.mock]")
	s = s[:i] + fmt.Sprintf(`[providers.weak]
kind = "mock"
family = "mock"
slots = 1
tiers = ["B"]
command = [%q]

[providers.strong]
kind = "mock"
family = "mock"
slots = 1
tiers = ["A"]
priority = 5
command = [%q]
`, weak, strong)
	if err := os.WriteFile(filepath.Join(prof, "swarm.toml"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	addTask(t, prof, "two.go,two_test.go")
	if out, err := swarm(t, prof, 3*time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if st := status(t, prof); !strings.Contains(st, "merged       1") {
		t.Fatalf("not merged:\n%s", st)
	}
	if got := sqlite(t, prof, "SELECT group_concat(provider, ',') FROM (SELECT provider FROM attempts WHERE task_id=1 ORDER BY id)"); got != "weak,weak,strong" {
		t.Fatalf("attempts by %q, want weak,weak,strong", got)
	}
	if n := sqlite(t, prof, "SELECT count(*) FROM events WHERE task_id=1 AND type='escalated'"); n != "1" {
		t.Fatalf("escalated events = %s, want 1", n)
	}
}

// With no tier-A provider, escalation must not strand the task: the third attempt runs on
// its own tier, and the task ends blocked by its attempt cap, not waiting forever.
func TestEscalationWithoutTierAKeepsWorking(t *testing.T) {
	repo := testbed(t)
	bad := `printf 'package app\n\nfunc Two() int { return 3 }\n' > two.go
printf 'package app\n\nimport "testing"\n\nfunc TestTwo(t *testing.T) { if Two() != 2 { t.Fatal() } }\n' > two_test.go
git add -A && git commit -qm "T$SWARM_TASK_ID: add Two" --allow-empty
swarm board done "$(git rev-parse HEAD)"
`
	prof := profile(t, repo, bad, 3)
	b, _ := os.ReadFile(filepath.Join(prof, "swarm.toml"))
	if err := os.WriteFile(filepath.Join(prof, "swarm.toml"), []byte(strings.Replace(string(b), "slots = 2", "slots = 2\ntiers = [\"B\"]", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	addTask(t, prof, "two.go,two_test.go")
	if out, err := swarm(t, prof, 3*time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if n := sqlite(t, prof, "SELECT count(*) FROM attempts WHERE task_id=1"); n != "3" {
		t.Fatalf("attempts = %s, want 3: the task stalled waiting for a tier nobody serves", n)
	}
	if st := status(t, prof); !strings.Contains(st, "blocked      1") {
		t.Fatalf("not blocked by its cap:\n%s", st)
	}
}
