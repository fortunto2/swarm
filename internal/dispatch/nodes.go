package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
	"github.com/fortunto2/swarm/internal/taskbase"
	"github.com/fortunto2/swarm/internal/worktree"
)

// Nodes are machines that dial in with `swarm node` and run the workers of their accounts
// there. The daemon stays the only writer of the database and the only one who merges: a
// node checks out, runs the CLI, pushes the task branch here when the worker reports, and
// runs the gate when the repo says gate_on = "node". Everything a node does is an order it
// took on a poll; everything it saw comes back as a report.

const (
	pollWait   = 25 * time.Second // a poll is held this long when there is nothing to do
	pushWait   = 3 * time.Minute  // report_done waits this long for the node to push the branch
	nodeEvents = 64
)

var errNodeLost = errors.New("the node went away with the worker running")

// nodeState: what the daemon knows of one connected node. Guarded by hub.mu.
type nodeState struct {
	session string
	root    string
	repos   map[string]bool
	version string
	proto   int // board.NodeProtocol it speaks; 1 when its hello did not say
	seen    time.Time
	admit   bool
	why     string
	warn    string // the node's own: what fails to stay in step
	live    []string
	cmds    []board.NodeCmd
	wake    chan struct{}
	polling int // polls held open right now: a node waiting on one is there, however long ago it began
}

// nodeEvent: a report the loop must apply (it owns the proc map), or, with live set, the
// node's list of what is running there (from a hello or a poll).
type nodeEvent struct {
	node string
	r    board.NodeReport
	live []string
	list bool
}

// liveGrace: a worker that left the node's live list gets this long for its exit report
// (sent right after) before the attempt counts as lost.
const liveGrace = 30 * time.Second

// startGrace: a start order the node never acknowledged (lost with a dead poll) is given up
// after this long.
const startGrace = 3 * time.Minute

// hub is the daemon's side of the node link; its methods run on RPC goroutines.
type hub struct {
	d      *Daemon
	mu     sync.Mutex
	nodes  map[string]*nodeState
	expect map[string]bool      // workers ordered to start and not yet finished
	pushes map[int64]chan error // report_done waiting for its branch
	gates  map[int64]gateWait   // a gate waiting for its node
}

// gateWait: the channel of a gate run on a node, and the commit it is for: a verdict for
// another commit (a gate the node kept running after a cancel) is not this one's.
type gateWait struct {
	commit string
	ch     chan gateMsg
}

// gateMsg: a node's verdict, or why it had none (v nil).
type gateMsg struct {
	v   *gate.Verdict
	why string
}

// maxGateAgain: gates a node could not give a verdict on, in a row, before the task is
// blocked for a person (a full disk, a wrong remote, a node retired for good).
const maxGateAgain = 5

func newHub(d *Daemon) *hub {
	return &hub{d: d, nodes: map[string]*nodeState{}, expect: map[string]bool{},
		pushes: map[int64]chan error{}, gates: map[int64]gateWait{}}
}

func newSession() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *hub) Hello(ctx context.Context, in board.NodeHello) (board.NodeWelcome, error) {
	cfg := h.d.Cfg
	nd, ok := cfg.Nodes[in.Name]
	if !ok || nd.Host != "remote" {
		return board.NodeWelcome{}, &board.Error{Code: "unknown_node", Hint: "no [nodes." + in.Name + "] with host = \"remote\" in the coordinator's profile"}
	}
	if in.Root == "" {
		return board.NodeWelcome{}, &board.Error{Code: "bad_input", Hint: "root (the node's worktree root) is required"}
	}
	w := board.NodeWelcome{Session: newSession(), Providers: map[string]config.Provider{}, Repos: map[string]board.RepoView{}}
	for _, a := range nd.Accounts {
		w.Providers[a] = cfg.RawProvider(a)
	}
	repos := map[string]bool{}
	for _, name := range in.Repos {
		r, ok := cfg.Repos[name]
		if !ok {
			continue // the node has a repo this profile does not: not ours to schedule
		}
		repos[name] = true
		w.Repos[name] = board.RepoView{MainRef: r.MainRef(), Merge: r.Merge, Prefix: r.BranchPrefix, Gate: r.Gate,
			GateTimeout: r.GateTimeout, Reruns: r.Reruns(), Review: r.Review}
	}
	for _, ref := range in.Worktrees {
		t, err := h.d.DB.GetTask(ctx, ref.Task)
		if err == nil && t.Repo == ref.Repo && (t.Status == db.Merged || t.Status == db.Cancelled) {
			w.Cleanup = append(w.Cleanup, ref)
		}
	}
	h.mu.Lock()
	old := h.nodes[in.Name]
	proto := max(in.Protocol, 1)
	n := &nodeState{session: w.Session, root: in.Root, repos: repos, version: in.Version, proto: proto, seen: time.Now(),
		live: in.Live, wake: make(chan struct{}, 1)}
	if old != nil {
		n.cmds = old.cmds // orders queued while it was away still stand
		close(old.wake)   // a poll of the old session ends now
	}
	for _, wk := range in.Live {
		if !h.expect[wk] {
			n.cmds = append(n.cmds, board.NodeCmd{Kind: "kill", Worker: wk})
		}
	}
	h.nodes[in.Name] = n
	h.mu.Unlock()
	h.d.DB.Event(ctx, "swarmd", 0, "node_hello", map[string]any{"node": in.Name, "version": in.Version, "repos": slices.Sorted(maps.Keys(repos))})
	h.record(ctx, in.Name)
	h.tellLoop(ctx, nodeEvent{node: in.Name, live: in.Live, list: true})
	return w, nil
}

// session finds the node a session belongs to. A stale session (the node reconnected, or
// the daemon restarted) is told so, and the node says hello again.
func (h *hub) session(s string) (string, *nodeState, error) {
	for name, n := range h.nodes {
		if n.session == s {
			return name, n, nil
		}
	}
	return "", nil, &board.Error{Code: "stale_session", Hint: "say hello again"}
}

// outdated: why a node of an older protocol takes no new work.
func outdated(proto int) string {
	return fmt.Sprintf("speaks node protocol %d, the coordinator %d: upgrade this node's swarm (new work waits)", proto, board.NodeProtocol)
}

func (h *hub) Poll(ctx context.Context, in board.NodePoll) (board.NodeOrders, error) {
	h.mu.Lock()
	name, n, err := h.session(in.Session)
	if err != nil {
		h.mu.Unlock()
		return board.NodeOrders{}, err
	}
	n.seen, n.admit, n.why, n.live, n.warn = time.Now(), in.Admit, in.Why, in.Live, in.Warn
	if n.proto < board.NodeProtocol { // its running work and gates go on; nothing new starts there
		n.admit, n.why = false, outdated(n.proto)
	}
	for _, wk := range in.Live { // a worker nobody here expects: an attempt that already ended
		if !h.expect[wk] && !slices.ContainsFunc(n.cmds, func(c board.NodeCmd) bool { return c.Kind == "kill" && c.Worker == wk }) {
			n.cmds = append(n.cmds, board.NodeCmd{Kind: "kill", Worker: wk})
		}
	}
	wait := min(time.Duration(in.Wait)*time.Second, pollWait)
	wake := n.wake
	n.polling++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		n.polling--
		h.mu.Unlock()
	}()
	h.record(ctx, name)
	h.cool(ctx, name, in.Cool)
	h.tellLoop(ctx, nodeEvent{node: name, live: in.Live, list: true})
	if wait > 0 {
		h.mu.Lock()
		empty := len(n.cmds) == 0
		h.mu.Unlock()
		if empty {
			select {
			case <-wake:
			case <-time.After(wait):
			case <-ctx.Done():
			}
		}
	}
	h.mu.Lock()
	cur, ok := h.nodes[name]
	if !ok || cur != n {
		h.mu.Unlock()
		return board.NodeOrders{}, &board.Error{Code: "stale_session", Hint: "say hello again"}
	}
	cmds := n.cmds
	n.cmds, n.seen = nil, time.Now()
	repos := slices.Sorted(maps.Keys(n.repos))
	h.mu.Unlock()
	out := board.NodeOrders{Cmds: cmds, Mains: map[string]string{}}
	for _, name := range repos {
		if r, err := h.d.repo(name); err == nil {
			if sha, err := gitx.Head(ctx, r.Path, r.MainRef()); err == nil {
				out.Mains[name] = sha
			}
		}
	}
	return out, nil
}

func (h *hub) Report(ctx context.Context, in board.NodeReport) error {
	h.mu.Lock()
	name, _, err := h.session(in.Session)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	h.nodes[name].seen = time.Now()
	switch in.Kind {
	case "pushed":
		if ch, ok := h.pushes[in.Task]; ok {
			delete(h.pushes, in.Task)
			var e error
			if in.Err != "" {
				e = errors.New(in.Err)
			}
			ch <- e
		}
		h.mu.Unlock()
		return nil
	case "gate", "gate_error":
		// gate_error: the node could not run it (fetch, checkout, disk): no verdict, and
		// nothing the task's worker did; nil makes the gate wait and try again
		if w, ok := h.gates[in.Task]; ok && w.commit == in.Commit {
			delete(h.gates, in.Task)
			m := gateMsg{why: in.Err}
			if in.Kind == "gate" {
				m.v = in.Verdict
			}
			w.ch <- m
		}
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	if in.Kind != "started" && in.Kind != "exited" && in.Kind != "failed" {
		return &board.Error{Code: "bad_input", Hint: "report kind " + in.Kind + " is not started, exited, failed, pushed, gate or gate_error"}
	}
	return h.tellLoop(ctx, nodeEvent{node: name, r: in})
}

func (h *hub) tellLoop(ctx context.Context, ev nodeEvent) error {
	select {
	case h.d.nodeEv <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// View: one row per configured remote node, for status.
func (h *hub) View() any {
	h.mu.Lock()
	defer h.mu.Unlock()
	type row struct {
		Node    string   `json:"node"`
		Online  bool     `json:"online"`
		Seen    string   `json:"seen,omitempty"`
		Admit   bool     `json:"admit"`
		Why     string   `json:"why,omitempty"`
		Running int      `json:"running"`
		Labels  []string `json:"labels,omitempty"`
	}
	var out []row
	for _, name := range slices.Sorted(maps.Keys(h.d.Cfg.Nodes)) {
		nd := h.d.Cfg.Nodes[name]
		if nd.Host != "remote" {
			continue
		}
		r := row{Node: name, Labels: nd.Labels}
		if n, ok := h.nodes[name]; ok {
			r.Online = n.polling > 0 || time.Since(n.seen) < h.d.Cfg.Limits.NodeTimeout.Duration
			r.Seen = n.seen.UTC().Format(time.RFC3339)
			r.Admit, r.Why, r.Running = n.admit, n.why, len(n.live)
		}
		out = append(out, r)
	}
	return out
}

// cool: accounts the node read as over their limit pause until the reset, as a local
// provider's limits file would make them.
func (h *hub) cool(ctx context.Context, node string, cs []board.Cool) {
	if len(cs) == 0 {
		return
	}
	states, err := h.d.DB.ProviderStates(ctx)
	if err != nil {
		return
	}
	for _, c := range cs {
		if h.d.Cfg.NodeOf(c.Provider) != node || c.Until <= time.Now().Unix() {
			continue
		}
		if st := states[c.Provider]; st.State == "cooling" || st.State == "disabled" {
			continue
		}
		if h.d.DB.CoolProvider(ctx, c.Provider, c.Until, c.Why) == nil {
			h.d.DB.Event(ctx, "swarmd", 0, "provider_cooling", map[string]string{"provider": c.Provider, "why": c.Why, "node": node})
		}
	}
}

// record writes the node's state for `swarm status`, on each hello and poll.
func (h *hub) record(ctx context.Context, name string) {
	h.mu.Lock()
	n, ok := h.nodes[name]
	if !ok {
		h.mu.Unlock()
		return
	}
	info := map[string]any{"admit": n.admit, "why": n.why, "warn": n.warn, "running": len(n.live), "version": n.version,
		"repos": slices.Sorted(maps.Keys(n.repos))}
	h.mu.Unlock()
	if err := h.d.DB.NodeSeen(ctx, name, info); err != nil {
		h.d.Log.Warn("node state not recorded", "node", name, "err", err)
	}
}

// order queues a command for a node and wakes its poll.
func (h *hub) order(node string, c board.NodeCmd) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, ok := h.nodes[node]
	if !ok {
		return // offline: hello's live list and cleanup cover what it missed
	}
	if c.Kind == "start" {
		h.expect[c.Worker] = true
	}
	if c.Kind == "status" { // only the latest move of a task matters to a board
		n.cmds = slices.DeleteFunc(n.cmds, func(o board.NodeCmd) bool { return o.Kind == "status" && o.Task == c.Task })
	}
	n.cmds = append(n.cmds, c)
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (h *hub) expected(worker string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.expect[worker]
}

func (h *hub) forget(worker string) {
	h.mu.Lock()
	delete(h.expect, worker)
	h.mu.Unlock()
}

// usable: a node that is connected and was heard from recently; admitting says whether its
// own limits.admit lets new work start there.
func (h *hub) usable(node string) (online, admitting bool, has func(repo string) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, ok := h.nodes[node]
	if !ok || (n.polling == 0 && time.Since(n.seen) > h.d.Cfg.Limits.NodeTimeout.Duration) {
		return false, false, func(string) bool { return false }
	}
	repos := n.repos
	return true, n.admit, func(r string) bool { return repos[r] }
}

// away: how long a node has not been heard from (forever if it never was).
func (h *hub) away(node string) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n, ok := h.nodes[node]; ok {
		if n.polling > 0 {
			return 0
		}
		return time.Since(n.seen)
	}
	return time.Duration(1 << 62)
}

func (h *hub) root(node string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n, ok := h.nodes[node]; ok {
		return n.root
	}
	return ""
}

// pushBranch asks the task's node to push the task branch here and waits for it.
func (h *hub) pushBranch(ctx context.Context, node string, t db.Task) error {
	ch := make(chan error, 1)
	h.mu.Lock()
	h.pushes[t.ID] = ch
	h.mu.Unlock()
	h.order(node, board.NodeCmd{Kind: "push", Task: t.ID, Repo: t.Repo})
	select {
	case err := <-ch:
		return err
	case <-time.After(pushWait):
	case <-ctx.Done():
	}
	h.mu.Lock()
	delete(h.pushes, t.ID)
	h.mu.Unlock()
	return fmt.Errorf("node %s did not push the branch within %s", node, pushWait)
}

// gateOn runs a gate on a node and waits for its verdict; ok is false when the node could
// not be reached in time, and the caller runs the gate here instead.
func (h *hub) gateOn(ctx context.Context, node string, t db.Task, commit, base string, limit time.Duration) (gate.Verdict, bool, string) {
	r, err := h.d.repo(t.Repo)
	if err != nil {
		return gate.Verdict{}, false, err.Error()
	}
	ref := fmt.Sprintf("refs/swarm/gate/T%d", t.ID)
	if _, err := gitx.Run(ctx, r.Path, "update-ref", ref, commit); err != nil {
		return gate.Verdict{}, false, err.Error()
	}
	ch := make(chan gateMsg, 1)
	h.mu.Lock()
	h.gates[t.ID] = gateWait{commit: commit, ch: ch}
	h.mu.Unlock()
	c := board.NodeCmd{Kind: "gate", Task: t.ID, Repo: t.Repo, Commit: commit, Ref: ref}
	if r.Review != "" { // the node's account reads it: it needs what was asked and where the diff starts
		c.Base, c.Title, c.Spec, c.Acceptance = base, t.Title, t.Spec, t.Acceptance
	}
	h.order(node, c)
	defer func() {
		h.mu.Lock()
		delete(h.gates, t.ID)
		h.mu.Unlock()
	}()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	deadline := time.After(limit)
	for {
		select {
		case m := <-ch:
			if m.v == nil {
				return gate.Verdict{}, false, "node " + node + " could not run it: " + m.why
			}
			return *m.v, true, ""
		case <-deadline:
			return gate.Verdict{}, false, "node " + node + " gave no verdict within " + limit.String()
		case <-ctx.Done():
			return gate.Verdict{Stage: "gate", Reason: "cancelled"}, true, ""
		case <-tick.C:
			if online, _, _ := h.usable(node); !online {
				return gate.Verdict{}, false, "node " + node + " went away mid-gate"
			}
		}
	}
}

// writeTail puts a node's log tail where a local attempt's log would be, so finish, logs
// and the gate-failure notes read it the same way.
func writeTail(path, tail string) {
	if tail != "" {
		_ = os.WriteFile(path, []byte(tail), 0o600)
	}
}

// onNode: the remote node a task's current attempt belongs to, or "" for this machine.
func (d *Daemon) onNode(t db.Task) string {
	if d.hub != nil && t.Node.Valid && d.Cfg.Remote(t.Node.String) {
		return t.Node.String
	}
	return ""
}

// verify (report_done): a local task's commit is checked in its worktree; a task on a node
// has its branch pushed here first, and is checked in the repo.
func (d *Daemon) verify(ctx context.Context, t db.Task) (string, error) {
	node := d.onNode(t)
	if node == "" {
		return d.locate(t).Path, nil
	}
	r, err := d.repo(t.Repo)
	if err != nil {
		return "", err
	}
	if err := d.hub.pushBranch(ctx, node, t); err != nil {
		return "", err
	}
	return r.Path, nil
}

// applyNode: a node said what happened to an attempt. Only the loop touches procs.
func (d *Daemon) applyNode(ctx context.Context, ev nodeEvent) {
	if ev.list {
		live := map[string]bool{}
		for _, w := range ev.live {
			live[w] = true
		}
		for _, p := range d.procs {
			if p.node != ev.node {
				continue
			}
			switch {
			case live[p.worker]:
				p.seenLive, p.missing = true, time.Time{}
			case p.seenLive && p.missing.IsZero():
				p.missing = time.Now() // gone from the node: its exit report has liveGrace to arrive
			}
		}
		return
	}
	var id int64
	var p *proc
	for tid, q := range d.procs {
		if q.node == ev.node && q.worker == ev.r.Worker {
			id, p = tid, q
			break
		}
	}
	if p == nil {
		if d.hub.expected(ev.r.Worker) {
			d.early[ev.r.Worker] = ev // its start is still on the way to the loop: applied after it
		}
		return // otherwise an attempt that already ended here; the next poll orders it stopped
	}
	switch ev.r.Kind {
	case "started":
		p.seenLive = true
		if ev.r.Commit != "" {
			_ = d.DB.SetBase(ctx, id, ev.r.Commit) // the base the node checked out from
		}
		d.DB.Event(ctx, "swarmd", id, "node_started", map[string]any{"node": ev.node, "pid": ev.r.Pid})
	case "exited", "failed":
		writeTail(p.log, d.redact.Apply(ev.r.Tail))
		var err error
		if ev.r.Err != "" {
			err = errors.New(ev.r.Err)
		} else if ev.r.Kind == "failed" {
			err = errors.New("the node could not start the worker")
		}
		select {
		case p.done <- err:
		default:
		}
	}
}

// prepareRemote: the coordinator's half of a start on a node. The base, the prompt and the
// token are made here; the checkout and the process are the node's. Quick: no git checkout.
func (d *Daemon) prepareRemote(ctx context.Context, t db.Task, node, pid, model, worker string, slot int) started {
	fail := func(err error) started { return started{taskID: t.ID, err: err} }
	r, err := d.repo(t.Repo)
	if err != nil {
		return fail(err)
	}
	base, baseRef, err := taskbase.For(ctx, d.DB, r, t)
	if err != nil {
		return fail(err)
	}
	// A worktree here from an earlier local attempt holds the task branch checked out, and
	// the node's push would be refused: the attempt moves, and starts on the node from there.
	if local := worktree.For(d.Cfg.Profile.WorktreeRoot, t.Repo, t.ID); dirExists(local.Path) {
		_, _ = gitx.Run(ctx, r.Path, "worktree", "remove", "--force", local.Path)
	}
	t.Node.String, t.Node.Valid = node, true // the prompt names the node's worktree
	dir := filepath.Join(d.Cfg.StatePath(), "attempts", worker)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	prompt, err := d.prompt(ctx, t, r, pid+"/"+model, node)
	if err != nil {
		return fail(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(prompt), 0o600)
	tok := d.Board.Issue(board.Identity{TaskID: t.ID, Role: "worker"})
	d.hub.order(node, board.NodeCmd{Kind: "start", Task: t.ID, Worker: worker, Repo: t.Repo, Base: base, Ref: baseRef,
		Prompt: prompt, Provider: pid, Model: model, Token: tok, Slot: slot, WallClock: d.Cfg.Limits.TaskWallClock})
	now := time.Now()
	return started{taskID: t.ID, model: model, p: &proc{node: node, worker: worker, token: tok, provider: pid, slot: slot,
		done: make(chan error, 1), log: filepath.Join(dir, "log.jsonl"), started: now,
		leaseUntil: now.Add(lease), deadline: now.Add(d.Cfg.Limits.TaskWallClock.Duration)}}
}

func dirExists(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }

// stop asks a worker to end: SIGTERM to its group here, a kill order to its node.
func (d *Daemon) stop(p *proc) {
	if p.node != "" {
		d.hub.order(p.node, board.NodeCmd{Kind: "kill", Worker: p.worker})
		return
	}
	killGroup(p.cmd)
}

// fits: may this provider take this task now, as far as its machine goes. The local node
// needs its own admission and capacity; a remote one must be connected, admitting, hold
// the repo, and have a free worker place. Labels must cover the task's runs_on, and a task
// stays on the node of its previous attempt while that node is there (its worktree is).
func (d *Daemon) fits(t db.Task, pid string, perNode map[string]int) bool {
	node := d.Cfg.NodeOf(pid)
	nd := d.Cfg.Nodes[node]
	if len(nd.Repos) > 0 && !slices.Contains(nd.Repos, t.Repo) {
		return false
	}
	for _, l := range t.Labels() {
		if !slices.Contains(nd.Labels, l) {
			return false
		}
	}
	if t.Node.Valid && t.Node.String != node && d.present(t.Node.String) {
		return false
	}
	if !d.Cfg.Remote(node) {
		return d.admitting() && perNode[node] < d.maxWorkers()
	}
	online, admitting, has := d.hub.usable(node)
	if !online || !admitting || !has(t.Repo) {
		return false
	}
	limit := nd.MaxWorkers
	if limit == 0 {
		for _, a := range nd.Accounts {
			limit += d.Cfg.Providers[a].Slots
		}
	}
	return perNode[node] < limit
}

// present: the local node always is; a remote one while it is connected.
func (d *Daemon) present(node string) bool {
	if !d.Cfg.Remote(node) {
		return node == d.Node
	}
	online, _, _ := d.hub.usable(node)
	return online
}

// cleanupRemote: after merge or cancel, the node may delete the task's worktree, and the
// branch it pushed here goes.
func (d *Daemon) cleanupRemote(ctx context.Context, t db.Task, r config.Repo) {
	_, _ = gitx.Run(ctx, r.Path, "update-ref", "-d", fmt.Sprintf("refs/swarm/gate/T%d", t.ID))
	if node := d.onNode(t); node != "" {
		d.hub.order(node, board.NodeCmd{Kind: "cleanup", Task: t.ID, Repo: t.Repo})
	}
}

// goneFromNode: a connected node no longer runs this attempt, or never started it, and no
// exit report came in the grace it was given (a report lost with a broken link, an order
// lost with a dead poll, a node restarted faster than node_timeout).
func (p *proc) goneFromNode(now time.Time) bool {
	return (!p.missing.IsZero() && now.Sub(p.missing) > liveGrace) || (!p.seenLive && now.Sub(p.started) > startGrace)
}
