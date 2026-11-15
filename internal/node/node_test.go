package node

import (
	"context"
	"io"
	"log/slog"
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
