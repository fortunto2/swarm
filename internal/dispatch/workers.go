package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	ids := make([]string, 0, len(d.adapters))
	for id := range d.adapters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	node, hasNode := d.Cfg.Nodes[d.Node]
	for _, id := range ids {
		p := d.Cfg.Providers[id]
		if len(p.Tiers) > 0 && !contains(p.Tiers, tier) {
			continue
		}
		if hasNode && len(node.Accounts) > 0 && !contains(node.Accounts, id) {
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
			d.infraFail(ctx, t.ID, "crash", err.Error())
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
	a := provider.Attempt{TaskID: t.ID, N: t.Attempts + 1, Worktree: wt.Path, Dir: dir, Prompt: prompt,
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
	d.procs[t.ID] = &proc{cmd: cmd, token: tok, provider: pid, done: done, log: logPath,
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
// live process holds (a daemon restart) once their lease has expired.
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
			<-p.done
			d.Board.Revoke(p.token)
			delete(d.procs, id)
			d.modelFail(ctx, id, "timeout", "wall clock "+d.Cfg.Limits.TaskWallClock.String()+" exceeded")
			continue
		}
		_ = d.DB.Set(ctx, id, map[string]any{"lease_until": now.Add(lease).Unix()})
	}
	running, err := d.DB.TasksIn(ctx, "running")
	if err != nil {
		return
	}
	for _, t := range running {
		if _, live := d.procs[t.ID]; !live && t.LeaseUntil.Valid && t.LeaseUntil.Int64 < now.Unix() {
			d.infraFail(ctx, t.ID, "crash", "lease expired with no live worker")
		}
	}
}

// finish: the process exited. If it reported, the task already moved on; if not, the
// worker died without a word, which is the infrastructure's failure, not the model's.
func (d *Daemon) finish(ctx context.Context, id int64, p *proc, err error) {
	d.Board.Revoke(p.token)
	delete(d.procs, id)
	if u := provider.ParseUsage(fileTail(p.log, 64<<10)); u.Known {
		d.DB.SetUsage(ctx, p.log, u.TokensIn, u.TokensOut, u.CostUSD)
	}
	t, gerr := d.DB.GetTask(ctx, id)
	if gerr != nil || t.Status != "running" {
		return
	}
	if provider.AuthFailure(fileTail(p.log, 8192)) {
		d.disableProvider(ctx, p.provider, "not logged in")
		d.DB.EndAttempt(ctx, id, "crash")
		_ = d.DB.Set(ctx, id, map[string]any{"status": "ready", "lease_until": nil})
		return
	}
	msg := "worker exited without report_done"
	if err != nil {
		msg += " (" + err.Error() + ")"
	}
	d.infraFail(ctx, id, "crash", msg)
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

func (d *Daemon) notify(msg string) {
	if d.Cfg.Profile.Notify == "" {
		return
	}
	_ = exec.Command("sh", "-c", d.Cfg.Profile.Notify+` "$1"`, "notify", msg).Run()
}

func fileTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := st.Size() - n
	if off < 0 {
		off = 0
	}
	b := make([]byte, st.Size()-off)
	_, _ = f.ReadAt(b, off)
	return string(b)
}

func (d *Daemon) infraFail(ctx context.Context, id int64, outcome, msg string) {
	t, err := d.DB.GetTask(ctx, id)
	if err != nil {
		return
	}
	d.DB.EndAttempt(ctx, id, outcome)
	status := "ready"
	if t.InfraFailures+1 >= maxInfraFailed {
		status = "blocked"
	}
	_ = d.DB.Set(ctx, id, map[string]any{"status": status, "infra_failures": t.InfraFailures + 1, "lease_until": nil})
	d.DB.Event(ctx, "swarmd", id, "infra_fail", map[string]string{"outcome": outcome, "msg": msg, "now": status})
	d.Log.Warn("infra failure", "task", id, "msg", msg, "status", status)
}

// modelFail counts against the model: gate or review failed, or it ran out of time.
func (d *Daemon) modelFail(ctx context.Context, id int64, outcome, detail string) {
	t, err := d.DB.GetTask(ctx, id)
	if err != nil {
		return
	}
	d.DB.EndAttempt(ctx, id, outcome)
	status := "ready"
	if t.Attempts+1 >= d.Cfg.Limits.MaxAttempts {
		status = "blocked" // phase 2: the lead decides split / rewrite / needs_human
	}
	_ = d.DB.Note(ctx, t.RunID, id, "swarmd", "review", fmt.Sprintf("attempt %d: %s\n%s", t.Attempts+1, outcome, detail))
	_ = d.DB.Set(ctx, id, map[string]any{"status": status, "attempts": t.Attempts + 1, "lease_until": nil})
	d.DB.Event(ctx, "swarmd", id, "model_fail", map[string]string{"outcome": outcome, "now": status})
	d.Log.Warn("attempt failed", "task", id, "outcome", outcome, "status", status)
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

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
