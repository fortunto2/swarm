package dispatch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
	"github.com/fortunto2/swarm/internal/worktree"
)

// startGates launches the gate for every verifying task not already being checked. Gates
// run off the tick so a 15-minute suite does not freeze scheduling.
func (d *Daemon) startGates(ctx context.Context) error {
	ts, err := d.DB.TasksIn(ctx, db.Verifying)
	if err != nil {
		return err
	}
	for _, t := range ts {
		if d.gating[t.ID] {
			continue
		}
		if len(d.gating) >= d.Cfg.Limits.MaxGates {
			return nil // ten suites at once time out honest work as gate_fail
		}
		r, err := d.repo(t.Repo)
		if err != nil {
			return err
		}
		main, err := gitx.Head(ctx, r.Path, "refs/heads/"+r.MainBranch)
		if err != nil {
			return err
		}
		claims, _ := d.DB.WriteClaims(ctx, t.ID)
		d.gating[t.ID] = true
		id, commit := t.ID, t.ResultCommit.String
		grants := gate.GrantsFrom(t.Spec)
		dir := filepath.Join(d.Cfg.StatePath(), "gates", fmt.Sprintf("T%d-%.12s", id, commit))
		gctx, cancel := context.WithCancel(ctx)
		d.gateCancel[id] = cancel
		go func() {
			defer cancel()
			v := gate.Static(gctx, r.Path, main, commit, claims, grants)
			if v.OK {
				v = runGate(gctx, r, dir, commit)
			}
			d.results <- gateResult{taskID: id, commit: commit, v: v}
		}()
		d.DB.Event(ctx, "swarmd", t.ID, "gate_started", map[string]string{"commit": commit})
	}
	return nil
}

// runGate checks the submitted commit in a throwaway detached worktree: the worker's own
// checkout (its untracked files, its build output, its HEAD) is never touched, and a
// re-gate after a rebase is the same operation.
func runGate(ctx context.Context, r config.Repo, dir, commit string) gate.Verdict {
	// a gate killed with the daemon leaves its worktree behind; clear it, or this check fails
	_, _ = gitx.Run(ctx, r.Path, "worktree", "remove", "--force", dir)
	_ = os.RemoveAll(dir)
	_, _ = gitx.Run(ctx, r.Path, "worktree", "prune")
	if _, err := gitx.Run(ctx, r.Path, "worktree", "add", "--quiet", "--detach", dir, commit); err != nil {
		return gate.Verdict{Stage: "gate", Reason: "could not check out the submitted commit", Tail: err.Error()}
	}
	defer func() { _, _ = gitx.Run(context.WithoutCancel(ctx), r.Path, "worktree", "remove", "--force", dir) }()
	return gate.Run(ctx, dir, r.Gate, r.GateTimeout.Duration, r.Reruns())
}

func (d *Daemon) applyGate(ctx context.Context, r gateResult) {
	delete(d.gating, r.taskID)
	delete(d.gateCancel, r.taskID)
	t, err := d.DB.GetTask(ctx, r.taskID)
	if err != nil || t.Status != db.Verifying || t.ResultCommit.String != r.commit {
		return // superseded: cancelled, or resubmitted while the gate ran
	}
	d.DB.Event(ctx, "swarmd", r.taskID, "gate_"+map[bool]string{true: "pass", false: "fail"}[r.v.OK],
		map[string]string{"stage": r.v.Stage, "reason": r.v.Reason})
	if len(r.v.Flaky) > 0 {
		d.DB.Event(ctx, "swarmd", r.taskID, "gate_flaky", map[string]any{"commands": r.v.Flaky})
		d.notify(fmt.Sprintf("swarm %s: flaky gate command on T%d (failed, then passed on rerun): %s",
			d.Cfg.Profile.Name, r.taskID, strings.Join(r.v.Flaky, "; ")))
	}
	if !r.v.OK {
		d.DB.Event(ctx, "swarmd", r.taskID, "gate_fail_detail", map[string]string{"tail": r.v.Tail})
		detail := r.v.Stage + ": " + r.v.Reason + "\n" + r.v.Tail
		if n, _ := d.DB.EventCountWith(ctx, r.taskID, "rebased", r.commit); n > 0 {
			detail = "this was your commit rebased by the daemon onto the current main; it fails there, " +
				"so rebase your branch onto main and fix it against the new code.\n" + detail
		}
		d.fail(ctx, r.taskID, db.Verifying, modelFail, "gate_fail", detail)
		return
	}
	// Cross-family review lands in phase 4; until then a green gate goes straight to the queue.
	if err := d.DB.Move(ctx, "swarmd", r.taskID, []db.Status{db.Verifying}, db.Merging, db.Patch{}); err != nil {
		return
	}
	d.Log.Info("gate passed", "task", r.taskID)
}

// mergeOne takes one task off the merge queue per tick. ff: rebase on main, re-gate if
// main moved, fast-forward. pr: leave the branch for a human (phase 6). A merge that fails
// moves its task to blocked with the reason: retrying the same fast-forward every tick
// would hold every task queued behind it.
func (d *Daemon) mergeOne(ctx context.Context) error {
	ts, err := d.DB.TasksIn(ctx, db.Merging)
	if err != nil || len(ts) == 0 {
		return err
	}
	t := ts[0]
	if err := d.merge(ctx, t); err != nil {
		d.blockMerge(ctx, t, err)
	}
	return nil
}

func (d *Daemon) merge(ctx context.Context, t db.Task) error {
	r, err := d.repo(t.Repo)
	if err != nil {
		return err
	}
	if r.Merge != "ff" {
		return fmt.Errorf("repo %s: merge %q lands in phase 6", t.Repo, r.Merge)
	}
	wt := d.locate(t)
	main, err := gitx.Head(ctx, r.Path, "refs/heads/"+r.MainBranch)
	if err != nil {
		return err
	}
	if !gitx.IsAncestor(ctx, r.Path, main, t.ResultCommit.String) {
		return d.rebase(ctx, t, r, main)
	}
	if cur, err := d.DB.GetTask(ctx, t.ID); err != nil || cur.Status != db.Merging {
		return nil // left the queue meanwhile
	}
	if err := ffMain(ctx, r, t.ResultCommit.String, main); err != nil {
		if now, herr := gitx.Head(ctx, r.Path, "refs/heads/"+r.MainBranch); herr == nil && now != main {
			return nil // someone moved main meanwhile: next tick rebases onto it
		}
		return err
	}
	if err := d.DB.Move(ctx, "swarmd", t.ID, []db.Status{db.Merging}, db.Merged, db.Patch{}); err != nil {
		// main already has the commit: never report this as a failed merge
		d.DB.Event(ctx, "swarmd", t.ID, "merged_status_not_recorded", map[string]string{"commit": t.ResultCommit.String, "err": err.Error()})
		d.Log.Error("merged, but the status move failed", "task", t.ID, "err", err)
		return nil
	}
	d.DB.Event(ctx, "swarmd", t.ID, "merged", map[string]string{"commit": t.ResultCommit.String})
	d.Log.Info("merged", "task", t.ID, "commit", t.ResultCommit.String)
	if err := worktree.Remove(ctx, r.Path, wt); err != nil {
		d.Log.Warn("worktree cleanup", "task", t.ID, "err", err)
	}
	d.bg.Add(1)
	go func() {
		defer d.bg.Done()
		d.afterMerge(context.WithoutCancel(ctx), t.ID, t.Repo, r.Path, t.ResultCommit.String)
	}()
	return nil
}

func (d *Daemon) blockMerge(ctx context.Context, t db.Task, err error) {
	if d.DB.Move(ctx, "swarmd", t.ID, []db.Status{db.Merging}, db.Blocked, db.Patch{}) != nil {
		return
	}
	_ = d.DB.Note(ctx, t.RunID, t.ID, "swarmd", "review", "merge failed: "+err.Error())
	d.DB.Event(ctx, "swarmd", t.ID, "merge_error", map[string]string{"err": err.Error()})
	d.Log.Error("merge failed, task blocked", "task", t.ID, "err", err)
	d.notify(fmt.Sprintf("swarm %s: T%d could not merge: %v", d.Cfg.Profile.Name, t.ID, err))
}

// afterMerge runs the profile's hooks, e.g. reindexing the repo so the next worker's code
// search sees what was just merged. Best effort, bounded, logged, off the loop.
func (d *Daemon) afterMerge(ctx context.Context, id int64, repo, path, commit string) {
	for _, h := range d.Cfg.Hooks.AfterMerge {
		cmdline := strings.NewReplacer("{repo}", repo, "{path}", path, "{task}", fmt.Sprint(id), "{commit}", commit).Replace(h)
		hctx, cancel := context.WithTimeout(ctx, d.Cfg.Hooks.Timeout.Duration)
		out, err := exec.CommandContext(hctx, "sh", "-c", cmdline).CombinedOutput()
		cancel()
		data := map[string]string{"cmd": cmdline}
		if err != nil {
			data["err"], data["out"] = err.Error(), gate.Tail(string(out), 1024)
			d.Log.Warn("after_merge hook failed", "task", id, "cmd", cmdline, "err", err)
		}
		d.DB.Event(ctx, "swarmd", id, "hook_after_merge", data)
	}
}

// maxRebaseConflicts: rebase conflicts send a task back to its worker without counting an
// attempt (PRD §9), but not forever: past this many, the task is blocked for a person.
const maxRebaseConflicts = 3

// rebase: main moved since the gate. The submitted commit is rebased in a scratch worktree
// the daemon owns, never in the worker's checkout (which may be dirty, or still in use).
// Clean: back through the gate on the new commit. Conflict: back to the worker, up to a
// cap. Any other git failure: blocked with the reason.
func (d *Daemon) rebase(ctx context.Context, t db.Task, r config.Repo, main string) error {
	dir := filepath.Join(d.Cfg.StatePath(), "rebase", fmt.Sprintf("T%d", t.ID))
	_, _ = gitx.Run(ctx, r.Path, "worktree", "remove", "--force", dir)
	_ = os.RemoveAll(dir)
	if _, err := gitx.Run(ctx, r.Path, "worktree", "add", "--quiet", "--detach", dir, t.ResultCommit.String); err != nil {
		return fmt.Errorf("rebase worktree: %w", err)
	}
	defer func() { _, _ = gitx.Run(context.WithoutCancel(ctx), r.Path, "worktree", "remove", "--force", dir) }()
	if _, err := gitx.Run(ctx, dir, "rebase", "--quiet", main); err != nil {
		// a conflict is what git says it is: unmerged paths, not a message to parse
		unmerged, _ := gitx.Run(ctx, dir, "diff", "--name-only", "--diff-filter=U")
		_, _ = gitx.Run(ctx, dir, "rebase", "--abort")
		if strings.TrimSpace(unmerged) == "" {
			return fmt.Errorf("rebase: %w", err) // not a conflict: blocked by the caller
		}
		n, err := d.DB.EventCountSinceRetry(ctx, t.ID, "rebase_conflict")
		if err != nil {
			return err
		}
		if n+1 >= maxRebaseConflicts {
			return fmt.Errorf("rebase onto main conflicted %d times", n+1)
		}
		if err := d.DB.Move(ctx, "swarmd", t.ID, []db.Status{db.Merging}, db.Ready, db.Patch{}); err != nil {
			return err
		}
		_ = d.DB.Note(ctx, t.RunID, t.ID, "swarmd", "review", "rebase onto main conflicts: rebase your branch on "+main+" and resolve")
		d.DB.Event(ctx, "swarmd", t.ID, "rebase_conflict", map[string]string{"main": main})
		return nil
	}
	head, err := gitx.Head(ctx, dir, "HEAD")
	if err != nil {
		return err
	}
	if err := d.DB.Move(ctx, "swarmd", t.ID, []db.Status{db.Merging}, db.Verifying, db.Patch{ResultCommit: &head, Base: &main}); err != nil {
		return err
	}
	d.DB.Event(ctx, "swarmd", t.ID, "rebased", map[string]string{"main": main, "commit": head})
	return nil
}

// ffMain moves main to the gated commit, never the branch tip (a worker may have committed
// after report_done), only as a fast-forward. When main is checked out in the
// repo, git merge --ff-only updates the working tree too (and refuses if it would clobber
// local edits); otherwise update-ref with the old value is a compare-and-swap.
func ffMain(ctx context.Context, r config.Repo, commit, oldMain string) error {
	if gitx.CurrentBranch(ctx, r.Path) == r.MainBranch {
		_, err := gitx.Run(ctx, r.Path, "merge", "--ff-only", "--quiet", commit)
		return err
	}
	_, err := gitx.Run(ctx, r.Path, "update-ref", "refs/heads/"+r.MainBranch, commit, oldMain)
	return err
}
