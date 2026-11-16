// Package node runs `swarm node`: a machine that dials a coordinator and works its orders.
// The coordinator decides what runs where and keeps the only database; a node checks out,
// runs the provider's CLI, pushes the task branch when the worker reports, runs a gate
// when asked, and says what happened. Workers here see an ordinary board socket: this
// process forwards their calls to the coordinator over the same link.
package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/dispatch"
	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
	"github.com/fortunto2/swarm/internal/jobs"
	"github.com/fortunto2/swarm/internal/provider"
	"github.com/fortunto2/swarm/internal/review"
	"github.com/fortunto2/swarm/internal/shellx"
	"github.com/fortunto2/swarm/internal/worktree"
)

const (
	pollWait      = 25 // seconds a poll may be held by the coordinator
	callLimit     = 60 * time.Second
	admitEvery    = 15 * time.Second
	syncEvery     = 2 * time.Minute // fetch/publish/deliveries, besides when main moves
	tailBytes     = 64 << 10
	transferLimit = 2 * time.Minute  // one fetch or push for an order; a stalled ssh must not hold it forever
	publishLimit  = 15 * time.Minute // a first publish of a big repo over a slow link (off the poll loop)
)

// Node is one `swarm node` process.
type Node struct {
	Cfg     *config.Config // the node profile
	Bin     string         // this swarm binary: workers call it for gate, board and mcp
	Version string
	Log     *slog.Logger
	// Dial opens the link; nil runs Cfg.Node.Coordinator + "relay --node". Tests replace it.
	Dial func(ctx context.Context) (io.ReadWriteCloser, string, error)

	mu       sync.Mutex
	syncing  sync.Mutex         // one keepInStep at a time
	moves    chan board.NodeCmd // status orders, run one at a time in the order they came
	doing    atomic.Int64       // orders taken and not yet done (a drain waits for them too)
	gating   map[string]bool    // gates running here, by task and commit: a repeated order waits for the first
	up       *rpc.Client
	token    string
	session  string
	welcome  board.NodeWelcome
	adapters map[string]provider.Adapter
	running  map[string]*attempt // by worker name
	mains    map[string]string   // coordinator main per repo, as last fetched
	syncErr  map[string]string   // by repo and step: the last failure of keeping in step, until it works again
	synced   time.Time
	admit    bool
	why      string
	asked    time.Time
	sock     string
}

// attempt: from the start order on, so the coordinator sees it live while it is prepared
// (a checkout can outlast its patience) and a second start of the task waits for it.
type attempt struct {
	task      int64
	cmd       *exec.Cmd // nil while it is prepared
	cancelled bool      // killed before its process existed
	done      chan struct{}
}

// Run serves the workers' socket and keeps the link up until ctx ends.
func (n *Node) Run(ctx context.Context) error {
	n.running, n.mains, n.gating = map[string]*attempt{}, map[string]string{}, map[string]bool{}
	n.moves = make(chan board.NodeCmd, 256)
	go func() { // one reader: a board never ends on an older status
		for c := range n.moves {
			n.statusHooks(ctx, c)
		}
	}()
	if n.Log == nil {
		n.Log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	if n.Dial == nil {
		n.Dial = n.dialCommand
	}
	n.sock = dispatch.SocketPath(n.Cfg.StatePath())
	if err := os.MkdirAll(filepath.Dir(n.sock), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(n.Cfg.StatePath(), 0o700); err != nil {
		return err
	}
	n.killOrphans()
	errc := make(chan error, 1)
	go func() { errc <- n.serveWorkers(ctx) }()
	backoff := time.Second
	_ = os.Remove(DrainFlag(n.Cfg.StatePath())) // a drain is one process's: this one takes work
	for ctx.Err() == nil {
		start := time.Now()
		err := n.session1(ctx)
		if errors.Is(err, errDrained) {
			n.Log.Info("drained: nothing runs here, exiting for a restart")
			_ = os.Remove(DrainFlag(n.Cfg.StatePath()))
			return nil
		}
		if ctx.Err() != nil {
			break
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second // it was up for a while: reconnect quickly
		}
		n.Log.Warn("link down, reconnecting", "err", err, "in", backoff.String())
		select {
		case <-ctx.Done():
		case err := <-errc:
			return err
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second) // a laptop waking up is back within half a minute
	}
	n.stopAll()
	return nil
}

// killOrphans: workers a node killed hard left running in their own process groups. They
// are stopped before this node takes new orders, as the daemon does after a crash.
func (n *Node) killOrphans() {
	dir := filepath.Join(n.Cfg.StatePath(), "live")
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		var pgid int
		var at int64
		if err == nil {
			if _, err := fmt.Sscan(string(b), &pgid, &at); err == nil && pgid > 0 {
				if dispatch.KillOrphan(pgid, time.Unix(at, 0)) {
					n.Log.Warn("orphan worker stopped", "worker", e.Name(), "pgid", pgid)
				}
			}
		}
		_ = os.Remove(p)
	}
}

// dialCommand runs the coordinator command with `relay --node`: its first line is the
// operator token, then the stream is the coordinator's node socket.
func (n *Node) dialCommand(ctx context.Context) (io.ReadWriteCloser, string, error) {
	argv := append(slices.Clone(n.Cfg.Node.Coordinator), "relay", "--node")
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, "", err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	br := bufio.NewReader(out)
	line, err := readLine(ctx, br)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, "", fmt.Errorf("coordinator command %v: %w", argv, err)
	}
	return &pipeConn{r: br, w: in, cmd: cmd}, line, nil
}

func readLine(ctx context.Context, br *bufio.Reader) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() { s, err := br.ReadString('\n'); ch <- res{strings.TrimSpace(s), err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		if r.s == "" {
			return "", errors.New("no token on the first line")
		}
		return r.s, nil
	case <-time.After(60 * time.Second):
		return "", errors.New("no answer in 60 s")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// pipeConn joins a command's stdout and stdin into one stream; Close ends the command.
type pipeConn struct {
	r   io.Reader
	w   io.WriteCloser
	cmd *exec.Cmd
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeConn) Close() error {
	_ = p.w.Close()
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	return nil
}

// session1 is one connected stretch: hello, then poll until the link breaks.
func (n *Node) session1(ctx context.Context) error {
	conn, token, err := n.Dial(ctx)
	if err != nil {
		return err
	}
	cl := jsonrpc.NewClient(conn)
	defer cl.Close()
	n.mu.Lock()
	n.up, n.token = cl, token
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		n.up = nil
		n.mu.Unlock()
	}()
	if err := n.hello(ctx); err != nil {
		return err
	}
	n.Log.Info("connected", "node", n.Cfg.Node.Name, "repos", slices.Sorted(maps.Keys(n.welcome.Repos)))
	for ctx.Err() == nil {
		if n.drained() {
			return errDrained
		}
		n.refreshAdmit(ctx)
		var orders board.NodeOrders
		n.mu.Lock()
		warn := n.syncWarnLocked()
		n.mu.Unlock()
		err := n.call(ctx, "node_poll", board.NodePoll{Session: n.session, Admit: n.admit, Why: n.why,
			Live: n.live(), Wait: pollWait, Cool: n.cooling(), Warn: warn}, &orders)
		if err != nil {
			var be *board.Error
			if errors.As(err, &be) && be.Code == "stale_session" {
				if err := n.hello(ctx); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if n.syncing.TryLock() { // off the poll loop: a stalled fetch must not stop the polls
			go func() { defer n.syncing.Unlock(); n.keepInStep(ctx, orders.Mains) }()
		}
		for _, c := range orders.Cmds {
			if c.Kind == "status" {
				select {
				case n.moves <- c:
				default: // hooks far behind: a board misses a move rather than the poll loop stalling
				}
				continue
			}
			n.doing.Add(1)
			go func(c board.NodeCmd) { defer n.doing.Add(-1); n.do(ctx, c) }(c)
		}
	}
	return ctx.Err()
}

// call: one operation on the coordinator, with the operator's token.
func (n *Node) call(ctx context.Context, op string, in, out any) error {
	n.mu.Lock()
	cl, tok := n.up, n.token
	n.mu.Unlock()
	if cl == nil {
		return errors.New("not connected")
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var r board.Reply
	c := cl.Go("Board.Do", board.Req{Op: op, Token: tok, Input: raw}, &r, make(chan *rpc.Call, 1))
	select {
	case <-c.Done:
	case <-time.After(callLimit + pollWait*time.Second):
		return errors.New(op + ": no answer")
	case <-ctx.Done():
		return ctx.Err()
	}
	if c.Error != nil {
		return c.Error
	}
	if r.Err != nil {
		return r.Err
	}
	if out != nil && len(r.Result) > 0 {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (n *Node) report(ctx context.Context, r board.NodeReport) {
	for try := 0; try < 5; try++ {
		n.mu.Lock()
		r.Session = n.session // a reconnect between tries makes a new session
		n.mu.Unlock()
		if err := n.call(ctx, "node_report", r, nil); err == nil {
			return
		} else if ctx.Err() != nil {
			return
		}
		time.Sleep(time.Duration(try+1) * 2 * time.Second)
	}
	n.Log.Warn("report lost", "kind", r.Kind, "task", r.Task)
}

func (n *Node) hello(ctx context.Context) error {
	in := board.NodeHello{Name: n.Cfg.Node.Name, Version: n.Version, Protocol: board.NodeProtocol, Root: n.Cfg.Profile.WorktreeRoot,
		Repos: slices.Sorted(maps.Keys(n.Cfg.Repos)), Live: n.live(), Worktrees: n.worktrees()}
	var w board.NodeWelcome
	if err := n.call(ctx, "node_hello", in, &w); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	adapters := map[string]provider.Adapter{}
	for id, p := range w.Providers {
		p.ConfigDir, p.Home, p.LimitsFile = config.Expand(p.ConfigDir), config.Expand(p.Home), config.Expand(p.LimitsFile)
		w.Providers[id] = p
		a, err := provider.New(id, p, n.Cfg.Profile)
		if err != nil {
			n.Log.Warn("provider unavailable here", "provider", id, "err", err)
			continue
		}
		adapters[id] = a
	}
	n.mu.Lock()
	n.session, n.welcome, n.adapters = w.Session, w, adapters
	n.mu.Unlock()
	for _, ref := range w.Cleanup { // off the poll loop: a cleanup may wait out a preparing attempt
		go n.cleanup(ctx, ref.Repo, ref.Task)
	}
	return nil
}

func (n *Node) live() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Sorted(maps.Keys(n.running))
}

var wtName = regexp.MustCompile(`^(.+)--T(\d+)$`)

// worktrees: tasks this node holds a worktree for, so the coordinator can say which can go.
func (n *Node) worktrees() []board.TaskRef {
	es, _ := os.ReadDir(n.Cfg.Profile.WorktreeRoot)
	var out []board.TaskRef
	for _, e := range es {
		m := wtName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if _, ok := n.Cfg.Repos[m[1]]; !ok {
			continue
		}
		if id, err := strconv.ParseInt(m[2], 10, 64); err == nil {
			out = append(out, board.TaskRef{Repo: m[1], Task: id})
		}
	}
	return out
}

// DrainFlag: `swarm drain` on a node profile leaves it; the node then takes no new work and
// exits once nothing runs, for its supervisor to start the new binary.
func DrainFlag(stateDir string) string { return filepath.Join(stateDir, "draining") }

// errDrained ends Run: the node drained and exits for a restart.
var errDrained = errors.New("drained")

func (n *Node) draining() bool {
	_, err := os.Stat(DrainFlag(n.Cfg.StatePath()))
	return err == nil
}

// drained: draining and nothing of ours left (no worker, no gate).
func (n *Node) drained() bool {
	if !n.draining() {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.doing.Load() == 0 && len(n.running) == 0 && len(n.gating) == 0
}

// refreshAdmit asks this machine's limits.admit, at most every 15 s.
func (n *Node) refreshAdmit(ctx context.Context) {
	if n.draining() {
		n.admit, n.why = false, "draining for a restart (swarm drain --cancel)"
		return
	}
	if n.Cfg.Limits.Admit == "" {
		n.admit, n.why = true, ""
		return
	}
	if time.Since(n.asked) < admitEvery {
		return
	}
	n.asked = time.Now()
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := shellx.Command(actx, n.Cfg.Limits.Admit).CombinedOutput()
	n.admit = err == nil
	n.why = strings.TrimSpace(gate.Tail(string(out), 200))
	if n.why == "" && err != nil {
		n.why = err.Error()
	}
}

// cooling: this node's accounts that their limits files say are over the line.
func (n *Node) cooling() []board.Cool {
	n.mu.Lock()
	ps := maps.Clone(n.welcome.Providers)
	n.mu.Unlock()
	var out []board.Cool
	now := time.Now()
	for _, id := range slices.Sorted(maps.Keys(ps)) {
		if until, why := dispatch.LimitsReading(ps[id], now); until > 0 {
			out = append(out, board.Cool{Provider: id, Until: until, Why: why})
		}
	}
	return out
}

// do carries out one order off the poll loop.
func (n *Node) do(ctx context.Context, c board.NodeCmd) {
	switch c.Kind {
	case "start":
		n.start(ctx, c)
	case "kill":
		n.kill(c.Worker)
	case "push":
		err := n.push(ctx, c)
		r := board.NodeReport{Kind: "pushed", Task: c.Task}
		if err != nil {
			r.Err = err.Error()
		}
		n.report(ctx, r)
	case "gate":
		n.gate(ctx, c)
	case "cleanup":
		n.cleanup(ctx, c.Repo, c.Task)
	case "job":
		n.runJob(ctx, c)

	default:
		n.Log.Warn("unknown order", "kind", c.Kind)
	}
}

func (n *Node) repo(name string) (config.Repo, board.RepoView, error) {
	r, ok := n.Cfg.Repos[name]
	n.mu.Lock()
	v, known := n.welcome.Repos[name]
	n.mu.Unlock()
	if !ok || !known {
		return r, v, fmt.Errorf("repo %s is not on this node", name)
	}
	return r, v, nil
}

// fetchMain makes the coordinator's main present here, as refs/swarm/main/<repo>.
func (n *Node) fetchMain(ctx context.Context, name string) error {
	_, v, err := n.repo(name)
	if err != nil {
		return err
	}
	return n.fetch(ctx, name, "+"+v.MainRef+":refs/swarm/main/"+name)
}

// fetchRef: a ref the coordinator keeps for one task (its joined base), to the same name here.
func (n *Node) fetchRef(ctx context.Context, name, ref string) error {
	if !strings.HasPrefix(ref, "refs/swarm/") {
		return fmt.Errorf("ref %q is not swarm's", ref)
	}
	return n.fetch(ctx, name, "+"+ref+":"+ref)
}

func (n *Node) fetch(ctx context.Context, name, refspec string) error {
	r, _, err := n.repo(name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, transferLimit)
	defer cancel()
	_, err = gitx.Transfer(ctx, r.Path, "fetch", "--quiet", r.Remote, refspec)
	return err
}

func (n *Node) start(ctx context.Context, c board.NodeCmd) {
	at := &attempt{task: c.Task, done: make(chan struct{})}
	liveFile := filepath.Join(n.Cfg.StatePath(), "live", c.Worker)
	finish := func() {
		n.mu.Lock()
		delete(n.running, c.Worker)
		n.mu.Unlock()
		_ = os.Remove(liveFile)
		close(at.done)
	}
	fail := func(err error) {
		finish()
		n.Log.Error("start failed", "task", c.Task, "err", err)
		n.report(context.WithoutCancel(ctx), board.NodeReport{Kind: "failed", Task: c.Task, Worker: c.Worker, Err: err.Error()})
	}
	n.mu.Lock()
	for {
		var older *attempt
		var name string
		for w, a := range n.running { // one worker per task: an older attempt still here goes first
			if a.task == c.Task {
				older, name = a, w
				break
			}
		}
		if older == nil {
			break
		}
		n.mu.Unlock()
		n.kill(name)
		<-older.done
		n.mu.Lock()
	}
	n.running[c.Worker] = at // live from here: the coordinator sees it while it is prepared
	adapter := n.adapters[c.Provider]
	n.mu.Unlock()
	if adapter == nil {
		fail(fmt.Errorf("provider %s cannot run on this node (see its log)", c.Provider))
		return
	}
	r, v, err := n.repo(c.Repo)
	if err != nil {
		fail(err)
		return
	}
	if _, err := gitx.Head(ctx, r.Path, c.Base+"^{commit}"); err != nil {
		fetch := func() error { return n.fetchMain(ctx, c.Repo) }
		if c.Ref != "" { // main joined with the task's dependencies, kept on the coordinator
			fetch = func() error { return n.fetchRef(ctx, c.Repo, c.Ref) }
		}
		if err := fetch(); err != nil {
			fail(fmt.Errorf("fetch the base from %s: %w", r.Remote, err))
			return
		}
		if _, err := gitx.Head(ctx, r.Path, c.Base+"^{commit}"); err != nil {
			fail(fmt.Errorf("base %.12s is not on the coordinator's main or its ref %q", c.Base, c.Ref))
			return
		}
	}
	wt, created, err := worktree.Ensure(ctx, r.Path, n.Cfg.Profile.WorktreeRoot, c.Repo, c.Task, c.Base)
	if err != nil {
		fail(fmt.Errorf("worktree: %w", err))
		return
	}
	base := ""
	if created {
		base = c.Base
	} else if moved, err := worktree.Refresh(ctx, wt.Path, c.Base); err != nil {
		fail(fmt.Errorf("refresh worktree: %w", err))
		return
	} else if moved {
		base = c.Base // a retry, on main as it is now with its commits on top
	}
	dir := filepath.Join(n.Cfg.StatePath(), "attempts", c.Worker)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail(err)
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(c.Prompt), 0o600)
	a := provider.Attempt{TaskID: c.Task, Worktree: wt.Path, Dir: dir, Prompt: c.Prompt, Model: c.Model,
		Socket: n.sock, Token: c.Token, SwarmBin: n.Bin, ExtraMCP: n.Cfg.MCPFor("worker"), Gate: v.Gate, Slot: c.Slot,
		Deny: n.Cfg.WorkerDeny(), Sandbox: n.Cfg.Sandbox, Strict: n.Cfg.Strict()}
	cmd, err := adapter.Command(ctx, a)
	if err != nil {
		fail(err)
		return
	}
	logPath := filepath.Join(dir, "log.jsonl")
	n.mu.Lock()
	if at.cancelled { // told to stop while it was prepared
		n.mu.Unlock()
		fail(errors.New("stopped before its process started"))
		return
	}
	done, err := dispatch.Launch(cmd, logPath, c.Worker)
	if err == nil {
		at.cmd = cmd
	}
	n.mu.Unlock()
	if err != nil {
		fail(err)
		return
	}
	_ = os.MkdirAll(filepath.Dir(liveFile), 0o700)
	_ = os.WriteFile(liveFile, []byte(fmt.Sprintf("%d %d\n", cmd.Process.Pid, time.Now().Unix())), 0o600)
	n.report(ctx, board.NodeReport{Kind: "started", Task: c.Task, Worker: c.Worker, Pid: cmd.Process.Pid, Commit: base})
	n.Log.Info("worker started", "task", c.Task, "worker", c.Worker)
	var exitErr error
	limit := c.WallClock.Duration + 2*time.Minute // the coordinator kills first; this is the backstop
	if c.WallClock.Duration == 0 {
		limit = 24 * time.Hour
	}
	select {
	case exitErr = <-done:
	case <-time.After(limit):
		n.kill(c.Worker)
		exitErr = <-done
	}
	_ = os.Remove(filepath.Join(dir, "mcp.json")) // resolved secrets of extra servers go with the attempt
	finish()
	rep := board.NodeReport{Kind: "exited", Task: c.Task, Worker: c.Worker, Tail: dispatch.FileTail(logPath, tailBytes)}
	if exitErr != nil {
		rep.Err = exitErr.Error()
	}
	n.report(context.WithoutCancel(ctx), rep)
	n.Log.Info("worker exited", "task", c.Task, "worker", c.Worker, "err", exitErr)
}

// kill: SIGTERM to the worker's group, SIGKILL if it is still there after 10 s.
func (n *Node) kill(worker string) {
	n.mu.Lock()
	a, ok := n.running[worker]
	if ok && (a.cmd == nil || a.cmd.Process == nil) {
		a.cancelled = true // still being prepared: it stops before launching
	}
	n.mu.Unlock()
	if !ok || a.cmd == nil || a.cmd.Process == nil {
		return
	}
	pgid := a.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	go func() {
		select {
		case <-a.done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}()
}

func (n *Node) stopAll() {
	n.mu.Lock()
	ws := slices.Collect(maps.Keys(n.running))
	n.mu.Unlock()
	for _, w := range ws {
		n.kill(w)
	}
}

// push sends the task branch to the coordinator's copy, where report_done is checked.
func (n *Node) push(ctx context.Context, c board.NodeCmd) error {
	r, _, err := n.repo(c.Repo)
	if err != nil {
		return err
	}
	b := config.WorkerBranch(c.Task)
	ctx, cancel := context.WithTimeout(ctx, transferLimit)
	defer cancel()
	_, err = gitx.Transfer(ctx, r.Path, "push", "--quiet", "--force", r.Remote, "refs/heads/"+b+":refs/heads/"+b)
	return err
}

// gate runs the repo's gate commands on a commit fetched from the coordinator, in a
// throwaway worktree, and reports the verdict.
func (n *Node) gate(ctx context.Context, c board.NodeCmd) {
	key := fmt.Sprintf("%d-%s", c.Task, c.Commit)
	n.mu.Lock()
	if n.gating[key] {
		n.mu.Unlock()
		return // the same gate is running here (the coordinator asked again): its verdict answers both
	}
	n.gating[key] = true
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.gating, key)
		n.mu.Unlock()
	}()
	r, v, err := n.repo(c.Repo)
	verdict := func(vd gate.Verdict) {
		n.report(context.WithoutCancel(ctx), board.NodeReport{Kind: "gate", Task: c.Task, Commit: c.Commit, Verdict: &vd})
	}
	// this machine could not run it: not a verdict on the work, the coordinator tries again
	broken := func(why string, err error) {
		n.Log.Warn("gate could not run", "task", c.Task, "why", why, "err", err)
		n.report(context.WithoutCancel(ctx), board.NodeReport{Kind: "gate_error", Task: c.Task, Commit: c.Commit, Err: why + ": " + err.Error()})
	}
	if err != nil {
		broken("repo", err)
		return
	}
	local := fmt.Sprintf("refs/swarm/gate/T%d", c.Task)
	fctx, cancel := context.WithTimeout(ctx, transferLimit)
	_, err = gitx.Transfer(fctx, r.Path, "fetch", "--quiet", r.Remote, "+"+c.Ref+":"+local)
	cancel()
	if err != nil {
		broken("fetch the commit", err)
		return
	}
	dir := filepath.Join(n.Cfg.StatePath(), "gates", fmt.Sprintf("T%d-%.12s", c.Task, c.Commit))
	_, _ = gitx.Run(ctx, r.Path, "worktree", "remove", "--force", dir)
	_ = os.RemoveAll(dir)
	_, _ = gitx.Run(ctx, r.Path, "worktree", "prune")
	if _, err := gitx.Run(ctx, r.Path, "worktree", "add", "--quiet", "--detach", dir, c.Commit); err != nil {
		broken("check out the commit", err)
		return
	}
	defer func() { _, _ = gitx.Run(context.WithoutCancel(ctx), r.Path, "worktree", "remove", "--force", dir) }()
	n.Log.Info("gate started", "task", c.Task, "commit", c.Commit)
	vd := gate.Run(ctx, dir, v.Gate, v.GateTimeout.Duration, v.Reruns)
	if v.Review != "" && c.Base != "" {
		if _, err := gitx.Head(ctx, r.Path, c.Base+"^{commit}"); err != nil {
			_ = n.fetchMain(ctx, c.Repo) // the diff starts at the task's base: main, as a rule
		}
		s := review.Spec{Task: review.Task{ID: c.Task, Title: c.Title, Spec: c.Spec, Acceptance: c.Acceptance},
			Base: c.Base, Dir: dir, Reviewer: v.Review, Cache: filepath.Join(n.Cfg.StatePath(), "reviews")}
		n.mu.Lock()
		p := n.welcome.Providers[v.Review]
		n.mu.Unlock()
		if run, err := provider.Reviewer(v.Review, p, n.Cfg.Profile, n.Cfg.WorkerDeny()); err != nil {
			s.Run = func(context.Context, string, string) (string, error) { return "", err }
		} else {
			s.Run = run
		}
		vd = review.After(ctx, vd, s)
	}
	verdict(vd)
}

// runJob: a scheduled job the coordinator gave this node, in its copy of the repo (or the
// profile's directory) with this machine's environment; the report carries exit and tail.
func (n *Node) runJob(ctx context.Context, c board.NodeCmd) {
	dir := n.Cfg.Dir
	if r, ok := n.Cfg.Repos[c.Repo]; c.Repo != "" && ok {
		dir = config.Expand(r.Path)
	}
	timeout := time.Duration(c.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	n.Log.Info("job started", "job", c.Job, "run", c.RunID)
	exit, out := jobs.Exec(ctx, c.Command, dir, append(os.Environ(), "SWARM_JOB="+c.Job, "SWARM_PROFILE="+n.Cfg.Dir), timeout)
	n.Log.Info("job done", "job", c.Job, "run", c.RunID, "exit", exit)
	n.report(context.WithoutCancel(ctx), board.NodeReport{Kind: "job", Job: c.Job, RunID: c.RunID, Exit: exit, Tail: out})
}

// statusHooks runs this node's hooks.on_status for a task of its own: {task} {status}
// {title} {repo} {worktree} (here), each bounded, best effort, as the daemon runs its own.
func (n *Node) statusHooks(ctx context.Context, c board.NodeCmd) {
	if len(n.Cfg.Hooks.OnStatus) == 0 {
		return
	}
	safe := dispatch.ShellSafe(c.Title)
	wt := worktree.For(n.Cfg.Profile.WorktreeRoot, c.Repo, c.Task).Path
	r := strings.NewReplacer("{task}", strconv.FormatInt(c.Task, 10), "{status}", c.Status, "{title}", safe,
		"{repo}", c.Repo, "{worktree}", wt)
	for _, h := range n.Cfg.Hooks.OnStatus {
		hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_ = shellx.Command(hctx, r.Replace(h)).Run()
		cancel()
	}
}

func (n *Node) cleanup(ctx context.Context, repo string, task int64) {
	n.mu.Lock()
	for w, a := range n.running { // a cancelled task's worker may still be there
		if a.task == task {
			n.mu.Unlock()
			n.kill(w)
			<-a.done
			n.mu.Lock()
		}
	}
	n.mu.Unlock()
	r, ok := n.Cfg.Repos[repo]
	if !ok {
		return
	}
	wt := worktree.For(n.Cfg.Profile.WorktreeRoot, repo, task)
	if _, err := os.Stat(wt.Path); err == nil {
		if err := worktree.Remove(ctx, r.Path, wt); err != nil {
			n.Log.Warn("cleanup", "task", task, "err", err)
		}
	}
	_, _ = gitx.Run(ctx, r.Path, "update-ref", "-d", fmt.Sprintf("refs/swarm/gate/T%d", task))
}

// keepInStep: the coordinator's main is fetched when it moves (a worker's `swarm gate`
// and a rebase need it here); a repo with land = "ff" fast-forwards its main to it; branch
// deliveries are fetched for the people on this machine; published refs are pushed.
func (n *Node) keepInStep(ctx context.Context, mains map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, publishLimit+2*transferLimit)
	defer cancel()
	periodic := time.Since(n.synced) > syncEvery
	if periodic {
		n.synced = time.Now()
	}
	for _, name := range slices.Sorted(maps.Keys(n.Cfg.Repos)) {
		r, v, err := n.repo(name)
		if err != nil {
			continue
		}
		moved := mains[name] != "" && mains[name] != n.mains[name]
		if moved {
			err := n.fetchMain(ctx, name)
			n.noteSync(name+" fetch main", err)
			if err == nil {
				n.mains[name] = mains[name]
				if r.Land == "ff" {
					n.land(ctx, name, r, mains[name])
				}
			}
		}
		if (moved || periodic) && v.Merge == "branch" && v.Prefix != "" {
			// non-forced: a local branch of the same name that moved on is left alone
			_, err := gitx.Transfer(ctx, r.Path, "fetch", "--quiet", r.Remote, "refs/heads/"+v.Prefix+"*:refs/heads/"+v.Prefix+"*")
			n.noteSync(name+" fetch deliveries", err)
		}
		if periodic {
			n.publish(ctx, name, r)
		}
	}
}

// land fast-forwards this node's main to the coordinator's, only when it is a fast-forward.
func (n *Node) land(ctx context.Context, name string, r config.Repo, sha string) {
	ref := "refs/heads/" + r.MainBranch
	cur, err := gitx.Head(ctx, r.Path, ref)
	if err != nil || cur == sha || !gitx.IsAncestor(ctx, r.Path, cur, sha) {
		return
	}
	if gitx.CurrentBranch(ctx, r.Path) == r.MainBranch {
		_, err = gitx.Run(ctx, r.Path, "merge", "--ff-only", "--quiet", sha)
	} else {
		_, err = gitx.Run(ctx, r.Path, "update-ref", ref, sha, cur)
	}
	if err != nil {
		n.Log.Warn("land", "repo", name, "err", err)
		return
	}
	n.Log.Info("landed", "repo", name, "main", sha)
}

// publish pushes this node's refs that the coordinator branches from (its main, say), and
// fetches first when the repo names a remote to fetch. Never forced: a coordinator copy
// that moved on is not rewound from here.
func (n *Node) publish(ctx context.Context, name string, r config.Repo) {
	if r.Fetch != "" {
		fctx, cancel := context.WithTimeout(ctx, transferLimit)
		_, _ = gitx.Transfer(fctx, r.Path, "fetch", "--quiet", r.Fetch)
		cancel()
	}
	for _, p := range r.Publish {
		pctx, cancel := context.WithTimeout(ctx, publishLimit)
		_, err := gitx.Transfer(pctx, r.Path, "push", "--quiet", r.Remote, p.From+":"+p.To)
		cancel()
		n.noteSync(name+" publish "+p.From, err)
		if err != nil {
			n.Log.Warn("publish", "repo", name, "ref", p.From, "err", err)
		}
	}
}

// noteSync records whether one step of keeping in step with the coordinator worked. A failing
// one is told to the coordinator in every poll and written to state/sync-warn for `swarm
// status` here: the link itself can stay up while new connections fail (seen live: an
// expired cloud login behind the ssh proxy; the node read online for an hour while its main
// stopped reaching the coordinator, so new tasks would branch from a stale one).
func (n *Node) noteSync(step string, err error) {
	n.mu.Lock()
	if n.syncErr == nil {
		n.syncErr = map[string]string{}
	}
	if err == nil {
		delete(n.syncErr, step)
	} else {
		n.syncErr[step] = errLine(err.Error())
	}
	warn := n.syncWarnLocked()
	n.mu.Unlock()
	path := SyncWarnFile(n.Cfg.StatePath())
	if warn == "" {
		_ = os.Remove(path)
		return
	}
	_ = os.WriteFile(path, []byte(warn+"\n"), 0o600)
}

// SyncWarnFile: where a node keeps what fails to stay in step, for `swarm status` there.
func SyncWarnFile(state string) string { return filepath.Join(state, "sync-warn") }

func (n *Node) syncWarnLocked() string {
	var parts []string
	for _, step := range slices.Sorted(maps.Keys(n.syncErr)) {
		parts = append(parts, step+": "+n.syncErr[step])
	}
	return strings.Join(parts, "; ")
}

// errLine: the line of a git/ssh error that says why ("ERROR", "fatal", "denied"), not the
// command echo; at most 160 characters.
func errLine(msg string) string {
	lines := strings.FieldsFunc(msg, func(r rune) bool { return r == '\n' || r == '\r' })
	pick := ""
	for _, l := range lines {
		ll := strings.ToLower(l)
		if strings.Contains(ll, "error") || strings.Contains(ll, "fatal") || strings.Contains(ll, "denied") ||
			strings.Contains(ll, "expired") || strings.Contains(ll, "timed out") || strings.Contains(ll, "refused") {
			pick = strings.TrimSpace(l)
			break
		}
	}
	if pick == "" && len(lines) > 0 {
		pick = strings.TrimSpace(lines[0])
	}
	if len(pick) > 160 {
		pick = pick[:160]
	}
	return pick
}

// serveWorkers: the board socket workers on this machine use, forwarded to the coordinator.
func (n *Node) serveWorkers(ctx context.Context) error {
	_ = os.Remove(n.sock)
	l, err := net.Listen("unix", n.sock)
	if err != nil {
		return err
	}
	_ = os.Chmod(n.sock, 0o600)
	srv := rpc.NewServer()
	if err := srv.RegisterName("Board", &forward{n: n}); err != nil {
		return err
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go srv.ServeCodec(jsonrpc.NewServerCodec(c))
	}
}

type forward struct{ n *Node }

// Do passes a worker's call through with its own token; while the link is down it gets
// the same answer as a stopped daemon.
func (f *forward) Do(r board.Req, out *board.Reply) error {
	f.n.mu.Lock()
	cl := f.n.up
	f.n.mu.Unlock()
	if cl == nil {
		out.Err = &board.Error{Code: "daemon_down", Hint: "this node has lost its coordinator for now; try again in a minute"}
		return nil
	}
	if err := cl.Call("Board.Do", r, out); err != nil {
		out.Err = &board.Error{Code: "daemon_down", Hint: "the link to the coordinator broke: " + err.Error()}
	}
	return nil
}

// Check dials the coordinator and asks its status without saying hello: no session, no
// orders, nothing a running node of the same name would notice. It proves the command,
// the relay, the token and that this node is declared there.
func (n *Node) Check(ctx context.Context) (string, error) {
	if n.Dial == nil {
		n.Dial = n.dialCommand
	}
	conn, token, err := n.Dial(ctx)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	cl := jsonrpc.NewClient(conn)
	defer cl.Close()
	n.mu.Lock()
	n.up, n.token = cl, token
	n.mu.Unlock()
	var st struct {
		Tasks map[string]int `json:"tasks"`
		Nodes []struct {
			Node   string `json:"node"`
			Online bool   `json:"online"`
		} `json:"nodes"`
	}
	if err := n.call(ctx, "swarm_status", struct{}{}, &st); err != nil {
		return "", fmt.Errorf("status: %w", err)
	}
	declared := false
	for _, x := range st.Nodes {
		declared = declared || x.Node == n.Cfg.Node.Name
	}
	if !declared {
		return "", fmt.Errorf("the coordinator answers, but has no [nodes.%s] with host = \"remote\"", n.Cfg.Node.Name)
	}
	return fmt.Sprintf("ok: the coordinator answers (tasks %v) and knows node %s", st.Tasks, n.Cfg.Node.Name), nil
}
