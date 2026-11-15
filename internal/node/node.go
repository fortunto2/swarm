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
	"syscall"
	"time"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/dispatch"
	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
	"github.com/fortunto2/swarm/internal/provider"
	"github.com/fortunto2/swarm/internal/shellx"
	"github.com/fortunto2/swarm/internal/worktree"
)

const (
	pollWait   = 25 // seconds a poll may be held by the coordinator
	callLimit  = 60 * time.Second
	admitEvery = 15 * time.Second
	syncEvery  = 2 * time.Minute // fetch/publish/deliveries, besides when main moves
	tailBytes  = 64 << 10
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
	up       *rpc.Client
	token    string
	session  string
	welcome  board.NodeWelcome
	adapters map[string]provider.Adapter
	running  map[string]*attempt // by worker name
	mains    map[string]string   // coordinator main per repo, as last fetched
	synced   time.Time
	admit    bool
	why      string
	asked    time.Time
	sock     string
}

type attempt struct {
	task int64
	cmd  *exec.Cmd
	done chan struct{}
}

// Run serves the workers' socket and keeps the link up until ctx ends.
func (n *Node) Run(ctx context.Context) error {
	n.running, n.mains = map[string]*attempt{}, map[string]string{}
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
	for ctx.Err() == nil {
		start := time.Now()
		err := n.session1(ctx)
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
		backoff = min(backoff*2, 2*time.Minute)
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
		n.refreshAdmit(ctx)
		var orders board.NodeOrders
		err := n.call(ctx, "node_poll", board.NodePoll{Session: n.session, Admit: n.admit, Why: n.why,
			Live: n.live(), Wait: pollWait, Cool: n.cooling()}, &orders)
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
		n.keepInStep(ctx, orders.Mains)
		for _, c := range orders.Cmds {
			go n.do(ctx, c)
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
	r.Session = n.session
	for try := 0; try < 3; try++ {
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
	in := board.NodeHello{Name: n.Cfg.Node.Name, Version: n.Version, Root: n.Cfg.Profile.WorktreeRoot,
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
	for _, id := range w.Cleanup {
		n.cleanup(ctx, id)
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
func (n *Node) worktrees() []int64 {
	es, _ := os.ReadDir(n.Cfg.Profile.WorktreeRoot)
	var out []int64
	for _, e := range es {
		m := wtName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if _, ok := n.Cfg.Repos[m[1]]; !ok {
			continue
		}
		if id, err := strconv.ParseInt(m[2], 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// refreshAdmit asks this machine's limits.admit, at most every 15 s.
func (n *Node) refreshAdmit(ctx context.Context) {
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
		n.cleanup(ctx, c.Task)
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
	r, v, err := n.repo(name)
	if err != nil {
		return err
	}
	_, err = gitx.Transfer(ctx, r.Path, "fetch", "--quiet", r.Remote, "+"+v.MainRef+":refs/swarm/main/"+name)
	return err
}

func (n *Node) start(ctx context.Context, c board.NodeCmd) {
	fail := func(err error) {
		n.Log.Error("start failed", "task", c.Task, "err", err)
		n.report(ctx, board.NodeReport{Kind: "failed", Task: c.Task, Worker: c.Worker, Err: err.Error()})
	}
	n.mu.Lock()
	for w, a := range n.running { // one worker per task: an older attempt still here goes first
		if a.task == c.Task {
			n.mu.Unlock()
			n.kill(w)
			<-a.done
			n.mu.Lock()
		}
	}
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
		if err := n.fetchMain(ctx, c.Repo); err != nil {
			fail(fmt.Errorf("fetch the base from %s: %w", r.Remote, err))
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
	} else if fresh, _ := dispatch.Untouched(ctx, wt.Path, c.Base); fresh {
		if _, err := gitx.Run(ctx, wt.Path, "reset", "--quiet", "--hard", c.Base); err != nil {
			fail(fmt.Errorf("refresh worktree: %w", err))
			return
		}
		base = c.Base
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
	done, err := dispatch.Launch(cmd, logPath, c.Worker)
	if err != nil {
		fail(err)
		return
	}
	at := &attempt{task: c.Task, cmd: cmd, done: make(chan struct{})}
	n.mu.Lock()
	n.running[c.Worker] = at
	n.mu.Unlock()
	liveFile := filepath.Join(n.Cfg.StatePath(), "live", c.Worker)
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
	n.mu.Lock()
	delete(n.running, c.Worker)
	n.mu.Unlock()
	_ = os.Remove(liveFile)
	close(at.done)
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
	n.mu.Unlock()
	if !ok || a.cmd.Process == nil {
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
	_, err = gitx.Transfer(ctx, r.Path, "push", "--quiet", "--force", r.Remote, "refs/heads/"+b+":refs/heads/"+b)
	return err
}

// gate runs the repo's gate commands on a commit fetched from the coordinator, in a
// throwaway worktree, and reports the verdict.
func (n *Node) gate(ctx context.Context, c board.NodeCmd) {
	r, v, err := n.repo(c.Repo)
	verdict := func(vd gate.Verdict) {
		n.report(context.WithoutCancel(ctx), board.NodeReport{Kind: "gate", Task: c.Task, Commit: c.Commit, Verdict: &vd})
	}
	if err != nil {
		verdict(gate.Verdict{Stage: "gate", Reason: err.Error()})
		return
	}
	local := fmt.Sprintf("refs/swarm/gate/T%d", c.Task)
	if _, err := gitx.Transfer(ctx, r.Path, "fetch", "--quiet", r.Remote, "+"+c.Ref+":"+local); err != nil {
		verdict(gate.Verdict{Stage: "gate", Reason: "could not fetch the commit to gate", Tail: err.Error()})
		return
	}
	dir := filepath.Join(n.Cfg.StatePath(), "gates", fmt.Sprintf("T%d-%.12s", c.Task, c.Commit))
	_, _ = gitx.Run(ctx, r.Path, "worktree", "remove", "--force", dir)
	_ = os.RemoveAll(dir)
	_, _ = gitx.Run(ctx, r.Path, "worktree", "prune")
	if _, err := gitx.Run(ctx, r.Path, "worktree", "add", "--quiet", "--detach", dir, c.Commit); err != nil {
		verdict(gate.Verdict{Stage: "gate", Reason: "could not check out the submitted commit", Tail: err.Error()})
		return
	}
	defer func() { _, _ = gitx.Run(context.WithoutCancel(ctx), r.Path, "worktree", "remove", "--force", dir) }()
	n.Log.Info("gate started", "task", c.Task, "commit", c.Commit)
	verdict(gate.Run(ctx, dir, v.Gate, v.GateTimeout.Duration, v.Reruns))
}

func (n *Node) cleanup(ctx context.Context, task int64) {
	for name, r := range n.Cfg.Repos {
		wt := worktree.For(n.Cfg.Profile.WorktreeRoot, name, task)
		if _, err := os.Stat(wt.Path); err != nil {
			continue
		}
		if err := worktree.Remove(ctx, r.Path, wt); err != nil {
			n.Log.Warn("cleanup", "task", task, "err", err)
		}
		_, _ = gitx.Run(ctx, r.Path, "update-ref", "-d", fmt.Sprintf("refs/swarm/gate/T%d", task))
	}
}

// keepInStep: the coordinator's main is fetched when it moves (a worker's `swarm gate`
// and a rebase need it here); a repo with land = "ff" fast-forwards its main to it; branch
// deliveries are fetched for the people on this machine; published refs are pushed.
func (n *Node) keepInStep(ctx context.Context, mains map[string]string) {
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
			if err := n.fetchMain(ctx, name); err == nil {
				n.mains[name] = mains[name]
				if r.Land == "ff" {
					n.land(ctx, name, r, mains[name])
				}
			}
		}
		if (moved || periodic) && v.Merge == "branch" && v.Prefix != "" {
			// non-forced: a local branch of the same name that moved on is left alone
			_, _ = gitx.Transfer(ctx, r.Path, "fetch", "--quiet", r.Remote, "refs/heads/"+v.Prefix+"*:refs/heads/"+v.Prefix+"*")
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
		_, _ = gitx.Transfer(ctx, r.Path, "fetch", "--quiet", r.Fetch)
	}
	for _, p := range r.Publish {
		if _, err := gitx.Transfer(ctx, r.Path, "push", "--quiet", r.Remote, p.From+":"+p.To); err != nil {
			n.Log.Warn("publish", "repo", name, "ref", p.From, "err", err)
		}
	}
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
