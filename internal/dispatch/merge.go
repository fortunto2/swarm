package dispatch

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
	"github.com/fortunto2/swarm/internal/worktree"
)

// The spec may allow touching tests (a task that rewrites a test suite). Explicit marker,
// so a worker cannot grant itself the permission.
const allowTestsMarker = "[allow-test-changes]"

// startGates launches the gate for every verifying task not already being checked. Gates
// run off the tick so a 15-minute suite does not freeze scheduling.
func (d *Daemon) startGates(ctx context.Context) error {
	ts, err := d.DB.TasksIn(ctx, "verifying")
	if err != nil {
		return err
	}
	for _, t := range ts {
		if d.gating[t.ID] {
			continue
		}
		r, err := d.repo(t.Repo)
		if err != nil {
			return err
		}
		base, err := d.DB.RunBase(ctx, t.RunID)
		if err != nil {
			return err
		}
		claims, _ := d.DB.WriteClaims(ctx, t.ID)
		d.gating[t.ID] = true
		id, wt, commit := t.ID, t.Worktree.String, t.ResultCommit.String
		allow := strings.Contains(t.Spec, allowTestsMarker)
		go func() {
			v := gate.Static(ctx, wt, base, commit, claims, allow)
			if v.OK {
				if _, err := gitx.Run(ctx, wt, "checkout", "--quiet", "--detach", commit); err != nil {
					v = gate.Verdict{Stage: "gate", Reason: "checkout of the submitted commit failed", Tail: err.Error()}
				} else {
					v = gate.Run(ctx, wt, r.Gate, r.GateTimeout.Duration)
					_, _ = gitx.Run(ctx, wt, "checkout", "--quiet", fmt.Sprintf("swarm/T%d", id))
				}
			}
			d.results <- gateResult{taskID: id, commit: commit, v: v}
		}()
		d.DB.Event(ctx, "swarmd", t.ID, "gate_started", map[string]string{"commit": commit})
	}
	return nil
}

func (d *Daemon) applyGate(ctx context.Context, r gateResult) {
	delete(d.gating, r.taskID)
	t, err := d.DB.GetTask(ctx, r.taskID)
	if err != nil || t.Status != "verifying" || t.ResultCommit.String != r.commit {
		return // superseded: cancelled, or resubmitted while the gate ran
	}
	d.DB.Event(ctx, "swarmd", r.taskID, "gate_"+map[bool]string{true: "pass", false: "fail"}[r.v.OK],
		map[string]string{"stage": r.v.Stage, "reason": r.v.Reason})
	if !r.v.OK {
		d.DB.Event(ctx, "swarmd", r.taskID, "gate_fail_detail", map[string]string{"tail": r.v.Tail})
		d.modelFail(ctx, r.taskID, "gate_fail", r.v.Stage+": "+r.v.Reason+"\n"+r.v.Tail)
		return
	}
	// Cross-family review lands in phase 4; until then a green gate goes straight to the queue.
	_ = d.DB.Set(ctx, r.taskID, map[string]any{"status": "merging"})
	d.Log.Info("gate passed", "task", r.taskID)
}

// mergeOne takes one task off the merge queue per tick. ff: rebase on main, re-gate if
// main moved, fast-forward. pr: leave the branch for a human (phase 6).
func (d *Daemon) mergeOne(ctx context.Context) error {
	ts, err := d.DB.TasksIn(ctx, "merging")
	if err != nil || len(ts) == 0 {
		return err
	}
	t := ts[0]
	r, err := d.repo(t.Repo)
	if err != nil {
		return err
	}
	if r.Merge != "ff" {
		return fmt.Errorf("repo %s: merge %q lands in phase 6", t.Repo, r.Merge)
	}
	wt := worktree.WT{Path: t.Worktree.String, Branch: t.Branch.String}
	main, err := gitx.Head(ctx, r.Path, "refs/heads/"+r.MainBranch)
	if err != nil {
		return err
	}
	if !gitx.IsAncestor(ctx, wt.Path, main, t.ResultCommit.String) {
		return d.rebase(ctx, t.ID, wt, main)
	}
	if err := ffMain(ctx, r, wt.Branch, main); err != nil {
		d.DB.Event(ctx, "swarmd", t.ID, "merge_error", map[string]string{"err": err.Error()})
		return err
	}
	_ = d.DB.Set(ctx, t.ID, map[string]any{"status": "merged"})
	d.DB.Event(ctx, "swarmd", t.ID, "merged", map[string]string{"commit": t.ResultCommit.String})
	d.Log.Info("merged", "task", t.ID, "commit", t.ResultCommit.String)
	if err := worktree.Remove(ctx, r.Path, wt); err != nil {
		d.Log.Warn("worktree cleanup", "task", t.ID, "err", err)
	}
	d.afterMerge(ctx, t.ID, t.Repo, r.Path, t.ResultCommit.String)
	return nil
}

// afterMerge runs the profile's hooks, e.g. reindexing the repo so the next worker's code
// search sees what was just merged. Best effort, bounded, logged.
func (d *Daemon) afterMerge(ctx context.Context, id int64, repo, path, commit string) {
	for _, h := range d.Cfg.Hooks.AfterMerge {
		cmdline := strings.NewReplacer("{repo}", repo, "{path}", path, "{task}", fmt.Sprint(id), "{commit}", commit).Replace(h)
		hctx, cancel := context.WithTimeout(ctx, d.Cfg.Hooks.Timeout.Duration)
		out, err := exec.CommandContext(hctx, "sh", "-c", cmdline).CombinedOutput()
		cancel()
		data := map[string]string{"cmd": cmdline}
		if err != nil {
			data["err"], data["out"] = err.Error(), tailString(string(out), 1024)
			d.Log.Warn("after_merge hook failed", "task", id, "cmd", cmdline, "err", err)
		}
		d.DB.Event(ctx, "swarmd", id, "hook_after_merge", data)
	}
}

func tailString(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// rebase: main moved since the gate. A clean rebase goes back through the gate on the new
// commit; a conflict goes back to the worker without counting against it.
func (d *Daemon) rebase(ctx context.Context, id int64, wt worktree.WT, main string) error {
	if _, err := gitx.Run(ctx, wt.Path, "rebase", "--quiet", main); err != nil {
		_, _ = gitx.Run(ctx, wt.Path, "rebase", "--abort")
		t, _ := d.DB.GetTask(ctx, id)
		_ = d.DB.Note(ctx, t.RunID, id, "swarmd", "review", "rebase onto main conflicts: rebase your branch on "+main+" and resolve")
		_ = d.DB.Set(ctx, id, map[string]any{"status": "ready"})
		d.DB.Event(ctx, "swarmd", id, "rebase_conflict", map[string]string{"main": main})
		return nil
	}
	head, err := gitx.Head(ctx, wt.Path, "HEAD")
	if err != nil {
		return err
	}
	_ = d.DB.Set(ctx, id, map[string]any{"status": "verifying", "result_commit": head})
	d.DB.Event(ctx, "swarmd", id, "rebased", map[string]string{"main": main, "commit": head})
	return nil
}

// ffMain moves main to the branch only as a fast-forward. When main is checked out in the
// repo, git merge --ff-only updates the working tree too (and refuses if it would clobber
// local edits); otherwise update-ref with the old value is a compare-and-swap.
func ffMain(ctx context.Context, r config.Repo, branch, oldMain string) error {
	if gitx.CurrentBranch(ctx, r.Path) == r.MainBranch {
		_, err := gitx.Run(ctx, r.Path, "merge", "--ff-only", "--quiet", branch)
		return err
	}
	tip, err := gitx.Head(ctx, r.Path, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	_, err = gitx.Run(ctx, r.Path, "update-ref", "refs/heads/"+r.MainBranch, tip, oldMain)
	return err
}
