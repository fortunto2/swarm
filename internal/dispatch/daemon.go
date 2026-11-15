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
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
	"github.com/fortunto2/swarm/internal/provider"
	"github.com/fortunto2/swarm/internal/worktree"
)

const (
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

	adapters map[string]provider.Adapter
	procs    map[int64]*proc
	starting map[int64]starting // claimed, being prepared off the loop
	started  chan started
	gating   map[int64]bool
	results  chan gateResult
	wake     chan int64
	bg       sync.WaitGroup // after-merge hooks: bounded by their timeout, awaited on exit
}

func New(cfg *config.Config, d *db.DB, swarmBin string) (*Daemon, error) {
	wake := make(chan int64, 64)
	dm := &Daemon{
		Cfg: cfg, DB: d, SwarmBin: swarmBin, Node: "local",
		Board:    &board.Service{DB: d, Done: wake},
		Socket:   SocketPath(cfg.StatePath()),
		Log:      slog.New(slog.NewJSONHandler(os.Stderr, nil)),
		adapters: map[string]provider.Adapter{},
		procs:    map[int64]*proc{},
		starting: map[int64]starting{},
		started:  make(chan started, 16),
		gating:   map[int64]bool{},
		results:  make(chan gateResult, 16),
		wake:     wake,
	}
	for id, p := range cfg.Providers {
		a, err := provider.New(id, p, cfg.Profile)
		if err != nil {
			dm.Log.Warn("provider unavailable", "provider", id, "err", err)
			continue
		}
		dm.adapters[id] = a
	}
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
	d.Board.Repo = d.repo
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
			d.bg.Wait()
			return nil
		}
		select {
		case <-ctx.Done():
			d.stopAll()
			d.bg.Wait()
			return nil
		case err := <-errc:
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
	statuses := []db.Status{db.Running, db.Verifying, db.Review, db.Merging}
	if len(d.adapters) > 0 { // ready work with no provider left to run it waits for a person
		statuses = append(statuses, db.Ready)
	}
	ts, err := d.DB.TasksIn(ctx, statuses...)
	return err == nil && len(ts) == 0
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
		if err := d.DB.SetRunStatus(ctx, run, "done"); err != nil {
			return err
		}
		d.DB.Event(ctx, "swarmd", 0, "run_done", map[string]int64{"run": run})
		d.Log.Info("run done", "run", run)
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
