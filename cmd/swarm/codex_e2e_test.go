package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A provider of kind codex, run through a stand-in `codex` on PATH that checks how it was
// called (exec --json, the worktree as -C, the prompt on stdin, its account in CODEX_HOME,
// the board token in its environment) and then works like any worker: commit, swarm gate,
// report done. The real CLI's flags were checked against codex-cli 0.147 --help.
func TestCodexProviderReadyToMerged(t *testing.T) {
	repo := testbed(t)
	marks := t.TempDir()
	fake := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
set -e
echo "$@" > %[1]q/argv
cat > %[1]q/prompt
echo "$CODEX_HOME" > %[1]q/home
[ -n "$SWARM_TOKEN" ] || { echo '{"type":"turn.failed","error":{"message":"no token"}}'; exit 1; }
case "$*" in *"exec --json -C $SWARM_WORKTREE"*) ;; *) echo bad argv >&2; exit 1;; esac
`+goodWorker+`
echo '{"type":"turn.completed","usage":{"input_tokens":120,"output_tokens":30}}'
`, marks)
	if err := os.WriteFile(filepath.Join(fake, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	prof := profile(t, repo, "exit 1", 2)
	// the mock is the profile's default provider; codex takes the work by priority
	toml, _ := os.ReadFile(filepath.Join(prof, "swarm.toml"))
	toml = []byte(strings.Replace(string(toml), "[providers.mock]", fmt.Sprintf(`[providers.codex]
kind = "codex"
family = "openai"
slots = 1
priority = 0
home = %q

[providers.mock]
priority = 9`, filepath.Join(marks, "codex-home")), 1))
	if err := os.WriteFile(filepath.Join(prof, "swarm.toml"), toml, 0o600); err != nil {
		t.Fatal(err)
	}
	addTask(t, prof, "two.go,two_test.go")
	if out, err := swarm(t, prof, 2*time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if st := status(t, prof); !strings.Contains(st, "merged       1") {
		argv, _ := os.ReadFile(filepath.Join(marks, "argv"))
		t.Fatalf("task not merged:\n%s\nargv: %s", st, argv)
	}
	prompt, _ := os.ReadFile(filepath.Join(marks, "prompt"))
	home, _ := os.ReadFile(filepath.Join(marks, "home"))
	if !strings.Contains(string(prompt), "worker for task T1") || strings.TrimSpace(string(home)) != filepath.Join(marks, "codex-home") {
		t.Fatalf("prompt on stdin %q, CODEX_HOME %q", prompt[:min(len(prompt), 80)], home)
	}
	if msg := sh(t, repo, "git log -1 --format=%an main"); !strings.Contains(msg, "codex") {
		t.Fatalf("the commit does not carry the codex worker's identity: %s", msg)
	}
}
