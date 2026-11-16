package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A repo with a reviewer: the gate passes, the reviewer rejects the first change with a
// finding, the worker's next attempt gets that finding in its prompt, and the second change
// is approved and merges. The reviewer is a scripted provider (kind mock) reading its prompt
// on stdin, as a real one would.
func TestReviewRejectsThenApproves(t *testing.T) {
	repo := testbed(t)
	marks := t.TempDir()
	reviewer := filepath.Join(marks, "reviewer.sh")
	if err := os.WriteFile(reviewer, []byte(fmt.Sprintf(`#!/bin/sh
cat > %[1]q/prompt.$$
if [ ! -f %[1]q/rejected ]; then
  touch %[1]q/rejected
  printf 'two.go:3: Two has no doc comment the task asked for\nVERDICT: reject\n'
else
  printf 'VERDICT: approve\n'
fi
`, marks)), 0o755); err != nil {
		t.Fatal(err)
	}
	// each attempt: the change, and the review it got last time is in the prompt it was given
	worker := `printf 'package app\n\n// Two is two.\nfunc Two() int { return 2 }\n' > two.go
printf 'package app\n\nimport "testing"\n\nfunc TestTwo(t *testing.T) { if Two() != 2 { t.Fatal() } }\n' > two_test.go
cp "$SWARM_PROMPT_FILE" "` + marks + `/worker-prompt.$SWARM_TASK_ID.$$"
echo "$(date +%s%N)" >> two_test.go.stamp
git add two.go two_test.go && git commit -qm "T$SWARM_TASK_ID: add Two" --allow-empty
swarm board done "$(git rev-parse HEAD)"
`
	prof := profile(t, repo, worker, 3)
	toml, _ := os.ReadFile(filepath.Join(prof, "swarm.toml"))
	s := strings.Replace(string(toml), `merge = "ff"`, "merge = \"ff\"\nreview = \"rev\"", 1)
	s += fmt.Sprintf("\n[providers.rev]\nkind = \"mock\"\nfamily = \"other\"\nroles = [\"reviewer\"]\nslots = 1\ncommand = [%q]\n", reviewer)
	if err := os.WriteFile(filepath.Join(prof, "swarm.toml"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	addTask(t, prof, "two.go,two_test.go")
	if out, err := swarm(t, prof, 3*time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if st := status(t, prof); !strings.Contains(st, "merged       1") {
		t.Fatalf("task not merged after the review:\n%s", st)
	}
	prompts, _ := filepath.Glob(filepath.Join(marks, "prompt.*"))
	if len(prompts) != 2 {
		t.Fatalf("reviewer ran %d times, want 2 (reject, then approve)", len(prompts))
	}
	first, _ := os.ReadFile(prompts[0])
	if !strings.Contains(string(first), "Task T1: add Two") || !strings.Contains(string(first), "+func Two() int { return 2 }") {
		t.Fatalf("the reviewer's prompt lacks the task or the diff:\n%s", first)
	}
	wp, _ := filepath.Glob(filepath.Join(marks, "worker-prompt.1.*"))
	found := false
	for _, p := range wp {
		b, _ := os.ReadFile(p)
		found = found || strings.Contains(string(b), "Two has no doc comment the task asked for")
	}
	if len(wp) < 2 || !found {
		t.Fatalf("the worker's second attempt was not told the review's finding (%d prompts)", len(wp))
	}
	notes, _ := swarm(t, prof, 30*time.Second, "task", "notes", "1")
	if !strings.Contains(notes, "review: rejected by rev") {
		t.Fatalf("notes do not record the review:\n%s", notes)
	}
}
