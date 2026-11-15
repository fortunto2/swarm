// Package dispatch is swarmd's loop: deterministic code, no model. It hands out tasks,
// reaps workers, runs the gate and merges. Models only work on tasks and, later, decide as
// the lead.
package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
	"github.com/fortunto2/swarm/internal/provider"
	"github.com/fortunto2/swarm/internal/redact"
	"github.com/fortunto2/swarm/internal/worktree"
)

const (
	prepareTimeout = 10 * time.Minute
	tickEvery      = 2 * time.Second
	lease          = 2 * time.Minute // extended every tick while the process lives
	maxInfraFailed = 3               // a task the infrastructure fails 3 times goes to blocked, not round and round
)

type proc struct {
	cmd        *exec.Cmd
	token      string
	provider   string
	done       chan error
	deadline   time.Time
	leaseUntil time.Time
	slot       int
	killed     bool // the wall clock ran out; waiting for the group to die
	killedAt   time.Time
	started    time.Time
	log        string
}

type gateResult struct {
	taskID int64
	commit string
	v      gate.Verdict
}

type Daemon struct {
	Cfg      *config.Config
	DB       *db.DB
	Board    *board.Service
	Socket   string
	SwarmBin string
	Node     string
	Log      *slog.Logger

	adapters   map[string]provider.Adapter
	procs      map[int64]*proc
	starting   map[int64]starting // claimed, being prepared off the loop
	started    chan started
	gating     map[int64]bool
	gateCancel map[int64]context.CancelFunc
	results    chan gateResult
	wake       chan int64
	bg         sync.WaitGroup // after-merge hooks: bounded by their timeout, awaited on exit
	pruned     time.Time      // last git worktree prune

	prepCtx    context.Context // parent of every prepare; cancelled at shutdown
	prepCancel context.CancelFunc
	notifyMu   sync.Mutex
	closing    bool          // set before bg.Wait: no bg.Add after it
	reg        registry      // provider states and spend, refreshed each schedule pass
	redact     redact.Filter // text swarm stores or forwards goes through it
}

func New(cfg *config.Config, d *db.DB, swarmBin string) (*Daemon, error) {
	wake := make(chan int64, 64)
	dm := &Daemon{
		Cfg: cfg, DB: d, SwarmBin: swarmBin, Node: "local",
		Board:      &board.Service{DB: d, Done: wake},
		Socket:     SocketPath(cfg.StatePath()),
		Log:        slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		adapters:   map[string]provider.Adapter{},
		procs:      map[int64]*proc{},
		starting:   map[int64]starting{},
		started:    make(chan started, 16),
		gating:     map[int64]bool{},
		gateCancel: map[int64]context.CancelFunc{},
		results:    make(chan gateResult, 16),
		wake:       wake,
	}
	for id, p := range cfg.Providers {
		a, err := provider.New(id, p, cfg.Profile)
		if err != nil {
			dm.Log.Warn("provider unavailable", "provider", id, "err", err)
			continue
		}
		dm.adapters[id] = a
	}
	dm.prepCtx, dm.prepCancel = context.WithCancel(context.Background())
	dm.redact = redact.Filter(cfg.Profile.Redact)
	if len(dm.adapters) == 0 {
		return nil, fmt.Errorf("no usable provider in profile %s", cfg.Profile.Name)
	}
	return dm, nil
}

// Run serves the board and ticks until ctx ends, or, with untilIdle, until no run has work
// left and no worker is alive.
func (d *Daemon) Run(ctx context.Context, untilIdle bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(d.Socket), 0o700); err != nil {
		return err
	}
	op, err := board.OperatorToken(d.Cfg.StatePath())
	if err != nil {
		return err
	}
	d.Board.IssueFixed(op, board.Identity{Role: "lead"})
	d.Board.Locate = d.locate
	d.Board.Notify = d.notify
	d.Board.Redact = d.redact.Apply
	d.Board.Strict = d.Cfg.Strict()
	keepAwake()
	d.Board.Repo = d.repo
	d.Board.Providers = func(ctx context.Context) (any, error) { return ProviderViews(ctx, d.Cfg, d.DB) }
	back, err := d.DB.EnsureProviders(ctx, slices.Sorted(maps.Keys(d.Cfg.Providers)))
	if err != nil {
		return err
	}
	for _, id := range back {
		d.DB.Event(ctx, "swarmd", 0, "provider_reenabled", map[string]string{"provider": id, "why": "daemon restarted"})
	}
	if err := d.reconcile(ctx); err != nil {
		return err
	}
	d.Board.BaseFor = func(name string) (string, error) {
		r, err := d.repo(name)
		if err != nil {
			return "", err
		}
		return gitx.Head(context.Background(), r.Path, "refs/heads/"+r.MainBranch)
	}
	errc := make(chan error, 1)
	go func() { errc <- board.Serve(ctx, d.Socket, d.Board) }()
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		if err := d.Tick(ctx); err != nil {
			d.Log.Error("tick", "err", err)
		}
		if untilIdle && d.idle(ctx) {
			d.waitBackground()
			return nil
		}
		select {
		case <-ctx.Done():
			d.shutdown()
			d.waitBackground()
			return nil
		case err := <-errc:
			d.shutdown() // whatever ended the socket, no worker outlives the daemon
			d.waitBackground()
			if ctx.Err() != nil || err == nil {
				return nil
			}
			return fmt.Errorf("board socket: %w", err)
		case <-t.C:
		case <-d.wake:
		case r := <-d.results:
			d.applyGate(ctx, r)
		case s := <-d.started:
			d.applyStart(ctx, s)
		}
	}
}

// Tick is one pass of PRD §7: reap, promote, schedule, gate, merge, close runs.
func (d *Daemon) Tick(ctx context.Context) error {
	d.reap(ctx)
	if err := d.promote(ctx); err != nil {
		return err
	}
	if err := d.schedule(ctx); err != nil {
		return err
	}
	if err := d.startGates(ctx); err != nil {
		return err
	}
	if err := d.mergeOne(ctx); err != nil {
		return err
	}
	d.pruneHourly(ctx)
	return d.closeRuns(ctx)
}

// idle: nothing this daemon can move on its own. Blocked tasks wait for the lead (phase 2)
// or a person, and drafts wait on them; neither is work for the dispatcher.
func (d *Daemon) idle(ctx context.Context) bool {
	if len(d.procs) > 0 || len(d.starting) > 0 || len(d.gating) > 0 {
		return false
	}
	if err := d.promote(ctx); err != nil { // a merge this tick may have freed a draft
		return false
	}
	busy, err := d.DB.TasksIn(ctx, db.Running, db.Verifying, db.Review, db.Merging)
	if err != nil || len(busy) > 0 {
		return false
	}
	// Ready work counts only if some provider could take it now. With every provider for
	// its tier cooling, disabled or absent, it waits for time or a person, not for us.
	ready, err := d.DB.TasksIn(ctx, db.Ready)
	if err != nil {
		return false
	}
	_ = d.refreshRegistry(ctx)
	for _, t := range ready {
		if len(d.eligible(t.Tier)) > 0 {
			return false
		}
	}
	return true
}

func (d *Daemon) closeRuns(ctx context.Context) error {
	open, err := d.DB.OpenRuns(ctx)
	if err != nil {
		return err
	}
	for run, active := range open {
		if active > 0 {
			continue
		}
		closed, err := d.DB.CloseRunIfSettled(ctx, run)
		if err != nil {
			return err
		}
		if !closed {
			continue // a task arrived after the count
		}
		d.DB.Event(ctx, "swarmd", 0, "run_done", map[string]int64{"run": run})
		d.Log.Info("run done", "run", run)
		d.notify(fmt.Sprintf("swarm %s: run %d done", d.Cfg.Profile.Name, run))
	}
	return nil
}

// SocketPath is short and fixed per state dir: a unix socket path is capped near 104 bytes,
// and a profile under a long home or a macOS TMPDIR passes that easily. It lives in the
// user's cache dir (not a world-writable /tmp), which is short on both macOS and Linux.
func SocketPath(stateDir string) string {
	h := sha256.Sum256([]byte(stateDir))
	name := hex.EncodeToString(h[:6]) + ".sock"
	if dir, err := os.UserCacheDir(); err == nil && len(dir)+len("/swarm/")+len(name) < 100 {
		return filepath.Join(dir, "swarm", name)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("swarm-%d", os.Getuid()), name)
}

func (d *Daemon) repo(name string) (config.Repo, error) {
	r, ok := d.Cfg.Repos[name]
	if !ok {
		return r, fmt.Errorf("task names repo %q, which the profile does not have", name)
	}
	return r, nil
}

func (d *Daemon) locate(t db.Task) worktree.WT {
	return worktree.For(d.Cfg.Profile.WorktreeRoot, t.Repo, t.ID)
}

// reconcile: at start this daemon is the only authority for its node, so a task it shows
// as running with no worker alive is the previous daemon's, cut off by the restart. It goes
// back to ready now, not when its lease runs out, and the restart is not the task's fault:
// no attempt and no infra strike is counted. Verifying tasks need nothing: the gate map is
// empty, so they are gated again on the first tick.
func (d *Daemon) reconcile(ctx context.Context) error {
	// Workers a crashed daemon left behind live in their own process groups: stop them
	// before their tasks are handed to new workers in the same worktrees.
	orphans, err := d.DB.OpenAttemptPids(ctx, d.Node)
	if err != nil {
		return err
	}
	for id, o := range orphans {
		pgid := o.Pid
		// pids are reused: kill only a group whose leader is no older than the attempt
		if age, ok := processAge(pgid); !ok || age > time.Since(time.Unix(o.Started, 0))+time.Minute {
			continue
		}
		if syscall.Kill(-pgid, 0) == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			d.DB.Event(ctx, "swarmd", id, "orphan_killed", map[string]int{"pgid": pgid})
		}
	}
	running, err := d.DB.TasksIn(ctx, db.Running)
	if err != nil {
		return err
	}
	for _, t := range running {
		if t.Node.Valid && t.Node.String != d.Node {
			continue
		}
		if d.DB.Move(ctx, "swarmd", t.ID, []db.Status{db.Running}, db.Ready, db.Patch{ClearLease: true}) == nil {
			d.DB.EndAttempt(ctx, t.ID, "crash")
			d.DB.Event(ctx, "swarmd", t.ID, "reconciled", map[string]string{"why": "daemon restarted"})
			d.Log.Info("reconciled after restart", "task", t.ID)
		}
	}
	return nil
}

// keepAwake: a sleeping Mac drops sessions mid-run (PRD §12). caffeinate holds the machine
// awake for as long as this process lives, and exits with it.
func keepAwake() {
	if runtime.GOOS != "darwin" {
		return
	}
	_ = exec.Command("caffeinate", "-dimsu", "-w", strconv.Itoa(os.Getpid())).Start()
}

// pruneHourly drops worktree records whose directories are gone (PRD §8).
func (d *Daemon) pruneHourly(ctx context.Context) {
	if time.Since(d.pruned) < time.Hour {
		return
	}
	d.pruned = time.Now()
	for _, r := range d.Cfg.Repos {
		_, _ = gitx.Run(ctx, r.Path, "worktree", "prune")
	}
}

// Lock: one daemon per profile. The lock is an flock on <state>/daemon.lock, released by
// the kernel when the process dies, so a crash never leaves it stuck. A second daemon would
// unlink the first one's socket and write the same database.
func Lock(stateDir string) (release func(), err error) { return lock(stateDir, 20) }

func lock(stateDir string, tries int) (release func(), err error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var lerr error
	for i := 0; i <= tries; i++ { // a CLI probing Running() holds it for microseconds
		if i > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		if lerr = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lerr == nil {
			break
		}
	}
	if lerr != nil {
		f.Close()
		return nil, fmt.Errorf("another swarm daemon holds this profile (%s)", stateDir)
	}
	return func() { f.Close() }, nil
}

// Running reports whether a daemon holds the profile's lock.
func Running(stateDir string) bool {
	release, err := lock(stateDir, 0)
	if err != nil {
		return true
	}
	release()
	return false
}

// waitBackground: no notification may start once this begins (bg.Add after Wait races).
func (d *Daemon) waitBackground() {
	d.notifyMu.Lock()
	d.closing = true
	d.notifyMu.Unlock()
	d.bg.Wait()
}

// processAge: how long a process has run, from ps (etime: [[dd-]hh:]mm:ss).
func processAge(pid int) (time.Duration, bool) {
	out, err := exec.Command("ps", "-o", "etime=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	return parseEtime(strings.TrimSpace(string(out)))
}

func parseEtime(s string) (time.Duration, bool) {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	secs := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, false
		}
		secs = secs*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(secs)*time.Second, true
}
