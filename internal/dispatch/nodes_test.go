package dispatch

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/gate"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
)

// fits decides where a task may run: repos a machine takes, labels it has, the node of
// the task's previous attempt, a remote node's presence, admission and capacity.
func TestFitsMachineRules(t *testing.T) {
	cfg := &config.Config{
		Limits: config.Limits{MaxWorkers: 2, NodeTimeout: config.Duration{Duration: time.Minute}},
		Providers: map[string]config.Provider{
			"here": {Slots: 1}, "far": {Slots: 2}, "gone": {Slots: 1},
		},
		Nodes: map[string]config.Node{
			"box":  {Host: "local", Accounts: []string{"here"}, Labels: []string{"vpc"}, Repos: []string{"app"}},
			"mac":  {Host: "remote", Accounts: []string{"far"}, MaxWorkers: 1},
			"away": {Host: "remote", Accounts: []string{"gone"}},
		},
	}
	d := &Daemon{Cfg: cfg, Node: "box"}
	d.hub = newHub(d)
	d.hub.nodes["mac"] = &nodeState{seen: time.Now(), admit: true, repos: map[string]bool{"app": true, "tools": true}}
	task := func(repo, runsOn, node string) db.Task {
		t := db.Task{Repo: repo}
		if runsOn != "" {
			t.RunsOn = sql.NullString{String: runsOn, Valid: true}
		}
		if node != "" {
			t.Node = sql.NullString{String: node, Valid: true}
		}
		return t
	}
	none := map[string]int{}
	cases := []struct {
		name string
		t    db.Task
		pid  string
		busy map[string]int
		want bool
	}{
		{"local takes its repo", task("app", "", ""), "here", none, true},
		{"local does not take a repo outside nodes.repos", task("tools", "", ""), "here", none, false},
		{"remote takes a repo it said hello with", task("tools", "", ""), "far", none, true},
		{"a label the machine lacks", task("app", "vpc", ""), "far", none, false},
		{"a label the machine has", task("app", "vpc", ""), "here", none, true},
		{"remote at its max_workers", task("app", "", ""), "far", map[string]int{"mac": 1}, false},
		{"a node that never said hello", task("app", "", ""), "gone", none, false},
		{"stays on its previous node while that is there", task("app", "", "mac"), "here", none, false},
		{"moves when its previous node is gone", task("app", "", "away"), "here", none, true},
	}
	for _, c := range cases {
		if got := d.fits(c.t, c.pid, c.busy); got != c.want {
			t.Errorf("%s: fits = %v, want %v", c.name, got, c.want)
		}
	}
	d.hub.nodes["mac"].admit = false
	if d.fits(task("app", "", ""), "far", none) {
		t.Error("a remote node whose admit says no took work")
	}
	d.hub.nodes["mac"].admit, d.hub.nodes["mac"].seen = true, time.Now().Add(-2*time.Minute)
	if d.fits(task("app", "", ""), "far", none) {
		t.Error("a node silent past node_timeout took work")
	}
	d.hub.nodes["mac"].polling = 1 // its poll began long ago and is still held open: it is there
	if !d.fits(task("app", "", ""), "far", none) {
		t.Error("a node waiting on a held poll counts as gone")
	}
}

// A connected node's live list is the truth about what runs there: a worker that left it
// gets liveGrace for its exit report, a start never acknowledged gets startGrace.
func TestLiveListDecidesLostAttempts(t *testing.T) {
	cfg := &config.Config{Nodes: map[string]config.Node{"mac": {Host: "remote", Accounts: []string{"far"}}}}
	d := &Daemon{Cfg: cfg, procs: map[int64]*proc{}, early: map[string]nodeEvent{}}
	d.hub = newHub(d)
	now := time.Now()
	running := &proc{node: "mac", worker: "far-T1-1", started: now}
	silent := &proc{node: "mac", worker: "far-T2-1", started: now.Add(-startGrace - time.Second)}
	d.procs[1], d.procs[2] = running, silent
	d.applyNode(context.Background(), nodeEvent{node: "mac", live: []string{"far-T1-1"}, list: true})
	if !running.seenLive || running.goneFromNode(now) {
		t.Fatal("a worker in the live list is not running")
	}
	if !silent.goneFromNode(now) {
		t.Fatal("a start the node never acknowledged is kept past startGrace")
	}
	d.applyNode(context.Background(), nodeEvent{node: "mac", live: nil, list: true})
	if running.missing.IsZero() || running.goneFromNode(now.Add(liveGrace/2)) {
		t.Fatal("a worker gone from the list must get liveGrace for its exit report")
	}
	if !running.goneFromNode(now.Add(liveGrace + time.Second)) {
		t.Fatal("a worker gone from the list past liveGrace is still counted as running")
	}
	d.applyNode(context.Background(), nodeEvent{node: "mac", live: []string{"far-T1-1"}, list: true})
	if !running.missing.IsZero() {
		t.Fatal("back in the list: not missing")
	}
}

// A gate verdict is for one commit: a node still running an older gate of the task (kept
// going after a cancel) must not pass the new commit.
func TestStaleGateVerdictIsNotDelivered(t *testing.T) {
	d := &Daemon{Cfg: &config.Config{}}
	h := newHub(d)
	h.nodes["mac"] = &nodeState{session: "s1", seen: time.Now(), wake: make(chan struct{}, 1)}
	ch := make(chan gateMsg, 1)
	h.gates[7] = gateWait{commit: "bbb", ch: ch}
	ok := &gate.Verdict{OK: true}
	_ = h.Report(context.Background(), board.NodeReport{Session: "s1", Kind: "gate", Task: 7, Commit: "aaa", Verdict: ok})
	select {
	case <-ch:
		t.Fatal("a verdict for commit aaa was delivered to the gate of bbb")
	default:
	}
	_ = h.Report(context.Background(), board.NodeReport{Session: "s1", Kind: "gate", Task: 7, Commit: "bbb", Verdict: ok})
	if m := <-ch; m.v == nil || !m.v.OK {
		t.Fatal("the verdict for the right commit did not arrive")
	}
	h.gates[7] = gateWait{commit: "ccc", ch: ch}
	_ = h.Report(context.Background(), board.NodeReport{Session: "s1", Kind: "gate_error", Task: 7, Commit: "ccc", Err: "fetch"})
	if m := <-ch; m.v != nil || m.why != "fetch" {
		t.Fatalf("a gate the node could not run came back as a verdict, or without its reason: %+v", m)
	}
}

// Status orders to a node collapse to the latest per task: a node away for hours does not
// replay every move, and a board cannot end on an older one.
func TestStatusOrdersCollapsePerTask(t *testing.T) {
	d := &Daemon{Cfg: &config.Config{}}
	h := newHub(d)
	h.nodes["mac"] = &nodeState{wake: make(chan struct{}, 1)}
	for _, st := range []string{"running", "verifying", "merging"} {
		h.order("mac", board.NodeCmd{Kind: "status", Task: 7, Status: st})
	}
	h.order("mac", board.NodeCmd{Kind: "status", Task: 8, Status: "running"})
	h.order("mac", board.NodeCmd{Kind: "kill", Worker: "w"})
	var got []string
	for _, c := range h.nodes["mac"].cmds {
		got = append(got, fmt.Sprintf("%s:%d:%s", c.Kind, c.Task, c.Status))
	}
	if fmt.Sprint(got) != "[status:7:merging status:8:running kill:0:]" {
		t.Fatalf("queued: %v", got)
	}
}

// A node that gives no gate verdict maxGateAgain times in a row blocks the task for a
// person, with the node's reason, instead of verifying forever.
func TestGateWithoutVerdictBlocksAfterMaxAgain(t *testing.T) {
	sdb, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	ctx := context.Background()
	run, _ := sdb.CreateRun(ctx, "x", "", "base")
	id, _ := sdb.AddTask(ctx, db.NewTask{RunID: run, Repo: "app", Title: "T", Spec: "s", Acceptance: "a", Tier: "B"})
	commit := "c0ffee"
	if err := sdb.Move(ctx, "t", id, []db.Status{db.Ready}, db.Running, db.Patch{}); err != nil {
		t.Fatal(err)
	}
	if err := sdb.Move(ctx, "t", id, []db.Status{db.Running}, db.Verifying, db.Patch{ResultCommit: &commit}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Cfg: &config.Config{}, DB: sdb, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		gating: map[int64]bool{}, gateCancel: map[int64]context.CancelFunc{}, regate: map[int64]regate{}}
	// no-verdicts on another commit do not count toward this one's streak
	for i := 1; i < maxGateAgain; i++ {
		d.applyGate(ctx, gateResult{taskID: id, commit: "older", again: true, why: "went away"})
	}
	if task, _ := sdb.GetTask(ctx, id); task.Status != db.Verifying {
		t.Fatalf("no-verdicts for another commit blocked this one: %s", task.Status)
	}
	for i := 1; i <= maxGateAgain; i++ {
		d.applyGate(ctx, gateResult{taskID: id, commit: commit, again: true, why: "node mac could not run it: disk full"})
		task, _ := sdb.GetTask(ctx, id)
		if i < maxGateAgain && task.Status != db.Verifying {
			t.Fatalf("after %d: %s, want still verifying", i, task.Status)
		}
		if i == maxGateAgain && task.Status != db.Blocked {
			t.Fatalf("after %d: %s, want blocked", i, task.Status)
		}
	}
	notes, _ := sdb.Notes(ctx, id, "review", 1)
	if len(notes) == 0 || !strings.Contains(notes[0].Body, "disk full") {
		t.Fatalf("the block does not carry the node's reason: %+v", notes)
	}
}

// A node may delete a worktree only of a finished task of that repo: another profile
// sharing its worktree root can hold <otherrepo>--T<same id> (seen live: an old profile's
// harness--T1 went because this coordinator's T1, an app task, had merged).
func TestHelloCleansOnlyTheTasksOwnRepo(t *testing.T) {
	sdb, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	ctx := context.Background()
	run, _ := sdb.CreateRun(ctx, "x", "", "base")
	id, _ := sdb.AddTask(ctx, db.NewTask{RunID: run, Repo: "app", Title: "T", Spec: "s", Acceptance: "a", Tier: "B"})
	if err := sdb.Move(ctx, "t", id, []db.Status{db.Ready}, db.Cancelled, db.Patch{}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Repos: map[string]config.Repo{"app": {}}, Nodes: map[string]config.Node{"mac": {Host: "remote", Accounts: []string{"m"}}}}
	d := &Daemon{Cfg: cfg, DB: sdb, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.hub = newHub(d)
	d.nodeEv = make(chan nodeEvent, 4)
	w, err := d.hub.Hello(ctx, board.NodeHello{Name: "mac", Root: "/wt", Repos: []string{"app"},
		Worktrees: []board.TaskRef{{Repo: "harness", Task: id}, {Repo: "app", Task: id}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Cleanup) != 1 || w.Cleanup[0] != (board.TaskRef{Repo: "app", Task: id}) {
		t.Fatalf("cleanup = %+v, want only app T%d", w.Cleanup, id)
	}
}

func TestGateElsewhereOnlyForANamedNodeThatIsNotHere(t *testing.T) {
	for _, c := range []struct{ gateOn, where, want string }{
		{"", "box", ""}, {"coordinator", "box", ""}, {"node", "mac", ""},
		{"mac", "mac", ""}, {"mac", "box", "mac"}, {"mac", "", "mac"},
	} {
		if got := gateElsewhere(c.gateOn, c.where); got != c.want {
			t.Errorf("gateElsewhere(%q, %q) = %q, want %q", c.gateOn, c.where, got, c.want)
		}
	}
}
