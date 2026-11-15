package node

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
)

// An attempt is live from its start order: a kill that comes while it is still prepared
// (a checkout outlasting the coordinator's patience) stops it before it launches.
func TestKillWhilePreparingStopsTheLaunch(t *testing.T) {
	n := &Node{running: map[string]*attempt{}, gating: map[string]bool{}}
	at := &attempt{task: 1, done: make(chan struct{})}
	n.running["w-T1-1"] = at
	if got := n.live(); len(got) != 1 || got[0] != "w-T1-1" {
		t.Fatalf("a preparing attempt is not in the live list: %v", got)
	}
	n.kill("w-T1-1")
	if !at.cancelled {
		t.Fatal("a kill during preparation was lost")
	}
}

// The coordinator orders a gate again when it gave up waiting (a laptop slept mid-gate): a
// second run of the same commit must not start over the first one's directory.
func TestARepeatedGateOrderWaitsForTheFirst(t *testing.T) {
	n := &Node{Cfg: &config.Config{}, running: map[string]*attempt{}, gating: map[string]bool{"5-abc": true},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	start := time.Now()
	n.gate(context.Background(), board.NodeCmd{Kind: "gate", Task: 5, Commit: "abc", Repo: "app"})
	if time.Since(start) > time.Second {
		t.Fatal("the repeated order ran (it tried the repo and reported)")
	}
	if !n.gating["5-abc"] {
		t.Fatal("the repeated order cleared the running gate's mark")
	}
}

// A start order names the ref its base is kept under; a node fetches only swarm's own refs,
// so a coordinator cannot have it overwrite a branch of the person's repo.
func TestFetchRefTakesOnlySwarmRefs(t *testing.T) {
	n := &Node{}
	for _, ref := range []string{"refs/heads/main", "refs/remotes/origin/main", "HEAD"} {
		if err := n.fetchRef(context.Background(), "r", ref); err == nil || !strings.Contains(err.Error(), "not swarm's") {
			t.Fatalf("fetchRef(%q) = %v, want a refusal", ref, err)
		}
	}
}

// The link can hold while every new connection fails (an expired cloud login behind the ssh
// proxy, seen live): the node says what is out of step, in its polls and in a state file,
// and forgets it once the step works again.
func TestSyncFailuresAreToldAndForgotten(t *testing.T) {
	state := t.TempDir()
	n := &Node{Cfg: &config.Config{Profile: config.Profile{StateDir: state}}}
	n.Cfg.Dir = state
	sso := "git push --quiet box:app refs/heads/main:refs/heads/main: exit status 128: " +
		"aws: [ERROR]: Error when retrieving token from sso: Token has expired and refresh failed\n" +
		"Connection closed by UNKNOWN port 65535\r\nfatal: Could not read from remote repository."
	n.noteSync("app publish refs/heads/main", errors.New(sso))
	n.mu.Lock()
	warn := n.syncWarnLocked()
	n.mu.Unlock()
	if !strings.Contains(warn, "app publish refs/heads/main: ") || !strings.Contains(warn, "Token has expired") {
		t.Fatalf("warn = %q, want the step and the line that says why", warn)
	}
	b, err := os.ReadFile(SyncWarnFile(n.Cfg.StatePath()))
	if err != nil || !strings.Contains(string(b), "Token has expired") {
		t.Fatalf("state file: %q %v", b, err)
	}
	n.noteSync("app publish refs/heads/main", nil)
	if _, err := os.Stat(SyncWarnFile(n.Cfg.StatePath())); !os.IsNotExist(err) {
		t.Fatalf("the warning outlived a working step: %v", err)
	}
}
