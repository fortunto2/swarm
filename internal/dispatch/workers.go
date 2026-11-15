package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
	"github.com/fortunto2/swarm/internal/prompts"
	"github.com/fortunto2/swarm/internal/provider"
	"github.com/fortunto2/swarm/internal/worktree"
)

func (d *Daemon) promote(ctx context.Context) error { return d.DB.Promote(ctx) }

func (d *Daemon) maxWorkers() int {
	if n, ok := d.Cfg.Nodes[d.Node]; ok && n.MaxWorkers > 0 && n.MaxWorkers < d.Cfg.Limits.MaxWorkers {
		return n.MaxWorkers
	}
	return d.Cfg.Limits.MaxWorkers
}

func (d *Daemon) schedule(ctx context.Context) error {
	ready, err := d.DB.TasksIn(ctx, db.Ready)
	if err != nil || len(ready) == 0 {
		return err
	}
	inFlight, err := d.DB.ClaimsInFlight(ctx)
	if err != nil {
		return err
	}
	if err := d.refreshRegistry(ctx); err != nil {
		return err
	}
	busy := map[string]int{}
	for _, p := range d.procs {
		busy[p.provider]++
	}
	for _, st := range d.starting {
		busy[st.provider]++
	}
	now := time.Now().Unix()
	for _, t := range ready {
		if len(d.procs)+len(d.starting) >= d.maxWorkers() {
			return nil
		}
		if t.NotBefore.Valid && t.NotBefore.Int64 > now {
			continue
		}
		claims, err := d.DB.WriteClaims(ctx, t.ID)
		if err != nil {
			return err
		}
		if blocker := overlapping(claims, inFlight); blocker != 0 {
			continue // waits for T<blocker> to merge or fail: same files, PRD §7
		}
		pid, ok := d.pickProvider(t.Tier, busy)
		if !ok {
			continue
		}
		p := d.Cfg.Providers[pid]
		model := p.Models[t.Tier]
		worker := fmt.Sprintf("%s-T%d-%d", pid, t.ID, t.Attempts+t.InfraFailures+1)
		if err := d.DB.Claim(ctx, t.ID, pid, model, worker, d.Node, lease); err != nil {
			if errors.Is(err, db.ErrTaken) {
				continue
			}
			return err
		}
		busy[pid]++
		inFlight[t.ID] = claims
		slot := d.freeSlot()
		d.starting[t.ID] = starting{provider: pid, slot: slot}
		go func() { d.started <- d.prepare(ctx, t, pid, model, worker, slot) }()
	}
	return nil
}

// started: a worker prepared off the loop, or why it could not be.
type started struct {
	taskID int64
	p      *proc
	model  string
	err    error
}

// prepare does the slow part of a start off the loop: the worktree (a checkout of the whole
// repo), the prompt, the process. Only applyStart touches the daemon's maps.
func (d *Daemon) prepare(ctx context.Context, t db.Task, pid, model, worker string, slot int) started {
	fail := func(err error) started { return started{taskID: t.ID, err: err} }
	r, err := d.repo(t.Repo)
	if err != nil {
		return fail(err)
	}
	// A new worktree starts from main as it is now, so a task sees everything merged before
	// it (its dependencies first of all), and its diff is measured from there.
	base, err := gitx.Head(ctx, r.Path, "refs/heads/"+r.MainBranch)
	if err != nil {
		return fail(err)
	}
	wt, created, err := worktree.Ensure(ctx, r.Path, d.Cfg.Profile.WorktreeRoot, t.Repo, t.ID, base)
	if err != nil {
		return fail(fmt.Errorf("worktree: %w", err))
	}
	if created {
		if err := d.DB.SetBase(ctx, t.ID, base); err != nil {
			return fail(err)
		}
	}
	dir := filepath.Join(d.Cfg.StatePath(), "attempts", worker)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	prompt, err := d.prompt(ctx, t, r, pid+"/"+model)
	if err != nil {
		return fail(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(prompt), 0o600)
	tok := d.Board.Issue(board.Identity{TaskID: t.ID, Role: "worker"})
	a := provider.Attempt{TaskID: t.ID, Worktree: wt.Path, Dir: dir, Prompt: prompt,
		Model: model, Socket: d.Socket, Token: tok, SwarmBin: d.SwarmBin, ExtraMCP: d.Cfg.MCPFor("worker"), Gate: r.Gate, Slot: slot}
	cmd, err := d.adapters[pid].Command(ctx, a)
	if err != nil {
		d.Board.Revoke(tok)
		return fail(err)
	}
	logPath := filepath.Join(dir, "log.jsonl")
	done, err := d.launch(cmd, logPath, worker)
	if err != nil {
		d.Board.Revoke(tok)
		return fail(err)
	}
	now := time.Now()
	return started{taskID: t.ID, model: model, p: &proc{cmd: cmd, token: tok, provider: pid, slot: slot, done: done, log: logPath,
		leaseUntil: now.Add(lease), deadline: now.Add(d.Cfg.Limits.TaskWallClock.Duration)}}
}

func (d *Daemon) applyStart(ctx context.Context, s started) {
	pid := d.starting[s.taskID].provider
	delete(d.starting, s.taskID)
	if s.err != nil {
		d.Log.Error("start failed", "task", s.taskID, "err", s.err)
		d.fail(ctx, s.taskID, db.Running, infraFail, "crash", s.err.Error())
		return
	}
	d.procs[s.taskID] = s.p
	_, _ = d.DB.StartAttempt(ctx, s.taskID, pid, s.model, d.Node, s.p.log)
	d.DB.Event(ctx, "swarmd", s.taskID, "started", map[string]string{"provider": pid, "log": s.p.log, "node": d.Node})
	d.Log.Info("worker started", "task", s.taskID, "provider", pid)
}

// launch: own process group (stop = signal the group, no orphans), output streamed to a
// file and never held in memory, commits signed with the worker's identity.
func (d *Daemon) launch(cmd *exec.Cmd, logPath, worker string) (chan error, error) {
	f, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=swarm-"+worker, "GIT_COMMITTER_NAME=swarm-"+worker,
		"GIT_AUTHOR_EMAIL=swarm@local", "GIT_COMMITTER_EMAIL=swarm@local")
	cmd.Cancel = nil // the daemon kills the group itself; ctx cancel must not kill only the leader
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		f.Close()
		done <- err
	}()
	return done, nil
}

func (d *Daemon) prompt(ctx context.Context, t db.Task, r config.Repo, provider string) (string, error) {
	files, _ := d.DB.WriteClaims(ctx, t.ID)
	notes, _ := d.DB.Notes(ctx, t.ID, "review", 4)
	var prev []string
	for _, n := range notes {
		prev = append(prev, n.Body)
	}
	rules := ""
	if path := d.Cfg.Profile.ProjectRules; path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("project_rules: %w", err)
		}
		rules = string(b)
	}
	wt := d.locate(t)
	return prompts.RenderWorker(prompts.Worker{ID: t.ID, Worktree: wt.Path, Branch: wt.Branch,
		Title: t.Title, Spec: t.Spec, Acceptance: t.Acceptance, Files: files, Previous: prev, Gate: r.Gate, Rules: rules,
		Provider: provider})
}

// reap: finished processes, wall-clock overruns, lease renewal, and running tasks that no
// live process holds (a daemon restart) once their lease has expired. Never blocks: a
// worker that ignores SIGTERM is waited for off the loop.
func (d *Daemon) reap(ctx context.Context) {
	now := time.Now()
	for id, p := range d.procs {
		select {
		case err := <-p.done:
			d.finish(ctx, id, p, err)
			continue
		default:
		}
		if now.After(p.deadline) {
			killGroup(p.cmd)
			go func() { <-p.done }()
			d.release(id, p)
			d.fail(ctx, id, db.Running, modelFail, "timeout", "wall clock "+d.Cfg.Limits.TaskWallClock.String()+" exceeded")
			continue
		}
		if now.After(p.leaseUntil.Add(-lease / 2)) { // renew at half-life, not every tick
			p.leaseUntil = now.Add(lease)
			_ = d.DB.Renew(ctx, id, p.leaseUntil.Unix())
		}
	}
	running, err := d.DB.TasksIn(ctx, db.Running)
	if err != nil {
		return
	}
	for _, t := range running {
		_, live := d.procs[t.ID]
		_, starting := d.starting[t.ID]
		if !live && !starting && t.LeaseUntil.Valid && t.LeaseUntil.Int64 < now.Unix() {
			d.fail(ctx, t.ID, db.Running, infraFail, "crash", "lease expired with no live worker")
		}
	}
}

func (d *Daemon) release(id int64, p *proc) {
	d.Board.Revoke(p.token)
	delete(d.procs, id)
}

// finish: the process exited. Usage is recorded whatever happened. If it reported, the
// task already moved on; if it could not log in, the provider is out; otherwise the worker
// died without a word, which is the infrastructure's failure, not the model's.
func (d *Daemon) finish(ctx context.Context, id int64, p *proc, err error) {
	d.release(id, p)
	res := provider.ParseResult(fileTail(p.log, 64<<10))
	if res.Known {
		d.DB.SetUsage(ctx, p.log, res.TokensIn, res.TokensOut, res.CostUSD)
	}
	if res.AuthFailed {
		d.disableProvider(ctx, p.provider, "not logged in")
		if d.DB.Move(ctx, "swarmd", id, []db.Status{db.Running}, db.Ready, db.Patch{ClearLease: true}) == nil {
			d.DB.EndAttempt(ctx, id, "crash")
		}
		return
	}
	msg := "worker exited without report_done"
	if err != nil {
		msg += " (" + err.Error() + ")"
	}
	d.fail(ctx, id, db.Running, infraFail, "crash", msg) // a no-op if it reported: the task left running
}

// disableProvider takes a provider out of this daemon's rotation and says so. Its tasks
// wait for another provider; with none left they wait for a person.
func (d *Daemon) disableProvider(ctx context.Context, id, why string) {
	delete(d.adapters, id)
	_ = d.DB.DisableProvider(ctx, id, why)
	d.DB.Event(ctx, "swarmd", 0, "provider_disabled", map[string]string{"provider": id, "why": why})
	d.Log.Error("provider disabled", "provider", id, "why", why)
	d.notify(fmt.Sprintf("swarm %s: provider %s disabled: %s", d.Cfg.Profile.Name, id, why))
}

// notify runs the profile's notifier off the loop, bounded: a hung webhook must not stop
// the daemon.
func (d *Daemon) notify(msg string) {
	if d.Cfg.Profile.Notify == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "sh", "-c", d.Cfg.Profile.Notify+` "$1"`, "notify", msg).Run()
	}()
}

func fileTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Seek(-n, io.SeekEnd); err != nil {
		_, _ = f.Seek(0, io.SeekStart) // shorter than n
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

type failKind int

const (
	infraFail failKind = iota // crash, lost lease: not the model's fault, capped at maxInfraFailed
	modelFail                 // gate, review, wall clock: counts as an attempt, capped at max_attempts
)

// fail ends the open attempt and sends the task back to ready, or to blocked when its
// counter reaches the cap. A model failure leaves its detail for the next attempt's prompt.
// The move is guarded by `from`: if the task already left it (the worker reported in the
// meantime), the failure is stale and nothing is written.
func (d *Daemon) fail(ctx context.Context, id int64, from db.Status, kind failKind, outcome, detail string) {
	t, err := d.DB.GetTask(ctx, id)
	if err != nil || t.Status != from {
		return
	}
	n, limit, event := t.InfraFailures+1, maxInfraFailed, "infra_fail"
	patch := db.Patch{ClearLease: true, InfraFailures: &n}
	if kind == modelFail {
		n, limit, event = t.Attempts+1, d.Cfg.Limits.MaxAttempts, "model_fail"
		patch = db.Patch{ClearLease: true, Attempts: &n}
	}
	to := db.Ready
	if n >= limit {
		to = db.Blocked // phase 2: the lead decides split / rewrite / needs_human
	}
	if err := d.DB.Move(ctx, "swarmd", id, []db.Status{from}, to, patch); err != nil {
		d.Log.Info("failure not applied", "task", id, "err", err)
		return
	}
	d.DB.EndAttempt(ctx, id, outcome)
	if kind == modelFail {
		_ = d.DB.Note(ctx, t.RunID, id, "swarmd", "review", fmt.Sprintf("attempt %d: %s\n%s", n, outcome, detail))
	}
	d.DB.Event(ctx, "swarmd", id, event, map[string]string{"outcome": outcome, "detail": detail, "now": string(to)})
	d.Log.Warn(event, "task", id, "outcome", outcome, "status", to)
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	time.AfterFunc(10*time.Second, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
}

func (d *Daemon) stopAll() {
	for _, p := range d.procs {
		killGroup(p.cmd)
	}
}

// starting: a claimed task being prepared off the loop.
type starting struct {
	provider string
	slot     int
}

// freeSlot: the lowest slot number no live or starting worker holds. A slot gives a worker
// its own port range (PORT=20000+slot*10) and anything else that must not collide.
func (d *Daemon) freeSlot() int {
	used := map[int]bool{}
	for _, p := range d.procs {
		used[p.slot] = true
	}
	for _, st := range d.starting {
		used[st.slot] = true
	}
	for i := 0; ; i++ {
		if !used[i] {
			return i
		}
	}
}

// overlapping returns a task in flight whose claims overlap these, or 0. A task without
// claims declared no files and is not checked: declaring files is what buys the guarantee.
func overlapping(claims []string, inFlight map[int64][]string) int64 {
	for id, theirs := range inFlight {
		for _, a := range claims {
			for _, b := range theirs {
				if gate.Overlap(a, b) {
					return id
				}
			}
		}
	}
	return 0
}
