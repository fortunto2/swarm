package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/fortunto2/swarm/internal/board"
	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/prompts"
	"github.com/fortunto2/swarm/internal/provider"
	"github.com/fortunto2/swarm/internal/worktree"
)

// promote: draft tasks whose every dependency is merged become ready.
func (d *Daemon) promote(ctx context.Context) error {
	_, err := d.DB.ExecContext(ctx, `UPDATE tasks SET status='ready', updated_at=unixepoch()
		WHERE status='draft' AND NOT EXISTS (
		  SELECT 1 FROM deps JOIN tasks n ON n.id=deps.needs_id
		  WHERE deps.task_id=tasks.id AND n.status!='merged')`)
	return err
}

// pickProvider: a provider that serves the tier, is on this node and has a free slot.
// Cheapest-first and failed-before ordering arrive with several providers (phase 4).
func (d *Daemon) pickProvider(tier string, busy map[string]int) (string, bool) {
	ids := slices.Sorted(maps.Keys(d.adapters))
	node, hasNode := d.Cfg.Nodes[d.Node]
	for _, id := range ids {
		p := d.Cfg.Providers[id]
		if len(p.Tiers) > 0 && !slices.Contains(p.Tiers, tier) {
			continue
		}
		if hasNode && len(node.Accounts) > 0 && !slices.Contains(node.Accounts, id) {
			continue
		}
		if busy[id] < p.Slots {
			return id, true
		}
	}
	return "", false
}

func (d *Daemon) maxWorkers() int {
	if n, ok := d.Cfg.Nodes[d.Node]; ok && n.MaxWorkers > 0 && n.MaxWorkers < d.Cfg.Limits.MaxWorkers {
		return n.MaxWorkers
	}
	return d.Cfg.Limits.MaxWorkers
}

func (d *Daemon) schedule(ctx context.Context) error {
	ready, err := d.DB.TasksIn(ctx, "ready")
	if err != nil {
		return err
	}
	busy := map[string]int{}
	for _, p := range d.procs {
		busy[p.provider]++
	}
	now := time.Now().Unix()
	for _, t := range ready {
		if len(d.procs) >= d.maxWorkers() {
			return nil
		}
		if t.NotBefore.Valid && t.NotBefore.Int64 > now {
			continue
		}
		pid, ok := d.pickProvider(t.Tier, busy)
		if !ok {
			continue
		}
		if err := d.start(ctx, t, pid); err != nil {
			if errors.Is(err, db.ErrTaken) {
				continue
			}
			d.Log.Error("start failed", "task", t.ID, "err", err)
			d.fail(ctx, t.ID, infraFail, "crash", err.Error())
			continue
		}
		busy[pid]++
	}
	return nil
}

func (d *Daemon) start(ctx context.Context, t db.Task, pid string) error {
	p := d.Cfg.Providers[pid]
	model := p.Models[t.Tier]
	worker := fmt.Sprintf("%s-T%d-%d", pid, t.ID, t.Attempts+t.InfraFailures+1)
	if err := d.DB.Claim(ctx, t.ID, pid, model, worker, d.Node, lease); err != nil {
		return err
	}
	r, err := d.repo(t.Repo)
	if err != nil {
		return err
	}
	base, err := d.DB.RunBase(ctx, t.RunID)
	if err != nil {
		return err
	}
	wt, err := worktree.Ensure(ctx, r.Path, config.Expand(d.Cfg.Profile.WorktreeRoot), t.Repo, t.ID, base)
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if err := d.DB.Set(ctx, t.ID, map[string]any{"branch": wt.Branch, "worktree": wt.Path}); err != nil {
		return err
	}
	t.Branch.String, t.Worktree.String = wt.Branch, wt.Path

	dir := filepath.Join(d.Cfg.StatePath(), "attempts", worker)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tok := d.Board.Issue(board.Identity{TaskID: t.ID, Role: "worker"})
	prompt, err := d.prompt(ctx, t, r)
	if err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(prompt), 0o600)
	a := provider.Attempt{TaskID: t.ID, Worktree: wt.Path, Dir: dir, Prompt: prompt,
		Model: model, Socket: d.Socket, Token: tok, SwarmBin: d.SwarmBin, ExtraMCP: d.Cfg.MCPFor("worker"), Gate: r.Gate}
	cmd, err := d.adapters[pid].Command(ctx, a)
	if err != nil {
		d.Board.Revoke(tok)
		return err
	}
	logPath := filepath.Join(dir, "log.jsonl")
	done, err := d.launch(cmd, logPath, pid, worker)
	if err != nil {
		d.Board.Revoke(tok)
		return err
	}
	_, _ = d.DB.StartAttempt(ctx, t.ID, pid, model, d.Node, logPath)
	d.procs[t.ID] = &proc{cmd: cmd, token: tok, provider: pid, done: done, log: logPath, leaseUntil: time.Now().Add(lease),
		deadline: time.Now().Add(d.Cfg.Limits.TaskWallClock.Duration)}
	d.DB.Event(ctx, "swarmd", t.ID, "started", map[string]string{"provider": pid, "worker": worker, "node": d.Node})
	d.Log.Info("worker started", "task", t.ID, "provider", pid, "worker", worker)
	return nil
}

// launch: own process group (stop = signal the group, no orphans), output streamed to a
// file and never held in memory, commits signed with the worker's identity.
func (d *Daemon) launch(cmd *exec.Cmd, logPath, pid, worker string) (chan error, error) {
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

func (d *Daemon) prompt(ctx context.Context, t db.Task, r config.Repo) (string, error) {
	files, _ := d.DB.WriteClaims(ctx, t.ID)
	notes, _ := d.DB.Notes(ctx, t.ID, "review", 4)
	var prev []string
	for _, n := range notes {
		prev = append(prev, n.Body)
	}
	rules := ""
	if f := d.Cfg.Profile.ProjectRules; f != "" {
		path := config.Expand(f)
		if !filepath.IsAbs(path) {
			path = filepath.Join(d.Cfg.Dir, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("project_rules: %w", err)
		}
		rules = string(b)
	}
	return prompts.RenderWorker(prompts.Worker{ID: t.ID, Worktree: t.Worktree.String, Branch: t.Branch.String,
		Title: t.Title, Spec: t.Spec, Acceptance: t.Acceptance, Files: files, Previous: prev, Gate: r.Gate, Rules: rules})
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
			d.fail(ctx, id, modelFail, "timeout", "wall clock "+d.Cfg.Limits.TaskWallClock.String()+" exceeded")
			continue
		}
		if now.After(p.leaseUntil.Add(-lease / 2)) { // renew at half-life, not every tick
			p.leaseUntil = now.Add(lease)
			_ = d.DB.Set(ctx, id, map[string]any{"lease_until": p.leaseUntil.Unix()})
		}
	}
	running, err := d.DB.TasksIn(ctx, "running")
	if err != nil {
		return
	}
	for _, t := range running {
		if _, live := d.procs[t.ID]; !live && t.LeaseUntil.Valid && t.LeaseUntil.Int64 < now.Unix() {
			d.fail(ctx, t.ID, infraFail, "crash", "lease expired with no live worker")
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
	t, gerr := d.DB.GetTask(ctx, id)
	if gerr != nil || t.Status != "running" {
		return
	}
	if res.AuthFailed {
		d.disableProvider(ctx, p.provider, "not logged in")
		d.DB.EndAttempt(ctx, id, "crash")
		_ = d.DB.Set(ctx, id, map[string]any{"status": "ready", "lease_until": nil})
		return
	}
	msg := "worker exited without report_done"
	if err != nil {
		msg += " (" + err.Error() + ")"
	}
	d.fail(ctx, id, infraFail, "crash", msg)
}

// disableProvider takes a provider out of this daemon's rotation and says so. Its tasks
// wait for another provider; with none left they wait for a person.
func (d *Daemon) disableProvider(ctx context.Context, id, why string) {
	delete(d.adapters, id)
	_, _ = d.DB.ExecContext(ctx, `INSERT INTO providers(id, state, last_error) VALUES(?, 'disabled', ?)
		ON CONFLICT(id) DO UPDATE SET state='disabled', last_error=excluded.last_error`, id, why)
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
func (d *Daemon) fail(ctx context.Context, id int64, kind failKind, outcome, detail string) {
	t, err := d.DB.GetTask(ctx, id)
	if err != nil {
		return
	}
	d.DB.EndAttempt(ctx, id, outcome)
	col, n, limit, event := "infra_failures", t.InfraFailures+1, maxInfraFailed, "infra_fail"
	if kind == modelFail {
		col, n, limit, event = "attempts", t.Attempts+1, d.Cfg.Limits.MaxAttempts, "model_fail"
		_ = d.DB.Note(ctx, t.RunID, id, "swarmd", "review", fmt.Sprintf("attempt %d: %s\n%s", n, outcome, detail))
	}
	status := "ready"
	if n >= limit {
		status = "blocked" // phase 2: the lead decides split / rewrite / needs_human
	}
	_ = d.DB.Set(ctx, id, map[string]any{"status": status, col: n, "lease_until": nil})
	d.DB.Event(ctx, "swarmd", id, event, map[string]string{"outcome": outcome, "detail": detail, "now": status})
	d.Log.Warn(event, "task", id, "outcome", outcome, "status", status)
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
