// Package taskbase: the commit a task's work is measured from.
//
// With merge = "ff" that is main: a dependency merges into main before its dependents start.
// With merge = "branch" a dependency is delivered as a branch and main never gets it until a
// person lands it, so a dependent started from main would not see the code it depends on
// (found live: a task that needed another's change to the same file conflicted at merge).
// There the base is main plus every merged dependency's delivered commit that main lacks,
// joined without a worktree (git merge-tree) into a commit kept at refs/swarm/base/T<id>-<sha>
// for a node to fetch.
//
// The join is deterministic: the same main and deliveries give the same commit id, so the
// start, the gate's diff and the merge step all agree on one base and nothing is rebased
// over and over. A delivery that conflicts with the rest is an error, never a guess: a base
// missing main or another dependency is the bug this exists to prevent.
package taskbase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/gitx"
)

// ErrConflict: a dependency's delivery does not join cleanly with main and the others.
var ErrConflict = errors.New("dependency delivery conflicts")

// Ref: where a joined base is kept on the coordinator. The commit is in the name, so a node
// fetching one base is never handed another made after main moved.
func Ref(taskID int64, commit string) string {
	return fmt.Sprintf("%s%.12s", prefix(taskID), commit)
}

func prefix(taskID int64) string { return fmt.Sprintf("refs/swarm/base/T%d-", taskID) }

// For returns the base of task t in repo r, and the ref a node fetches it from ("" when the
// base is main itself).
func For(ctx context.Context, d *db.DB, r config.Repo, t db.Task) (string, string, error) {
	main, err := gitx.Head(ctx, r.Path, r.MainRef())
	if err != nil {
		return "", "", err
	}
	if r.Merge != "branch" {
		return main, "", nil
	}
	deliveries, err := d.DepDeliveries(ctx, t.ID, t.Repo)
	if err != nil {
		return "", "", err
	}
	base := main
	for _, dep := range deliveries {
		c := dep.Commit
		if _, err := gitx.Head(ctx, r.Path, c); err != nil {
			continue // not in this copy (pruned): main is all there is
		}
		switch {
		case gitx.IsAncestor(ctx, r.Path, c, base):
			continue // already in
		case gitx.IsAncestor(ctx, r.Path, base, c):
			base = c // the delivery is the base plus its own commits
		default:
			joined, err := gitx.Join(ctx, r.Path, base, c, fmt.Sprintf("swarm: base for T%d", t.ID))
			if errors.Is(err, gitx.ErrConflict) {
				return "", "", fmt.Errorf("%w: T%d's delivery %.12s does not join with %s and the earlier dependencies; "+
					"land or rebase T%d, then swarm task retry %d", ErrConflict, dep.Task, c, r.MainRef(), dep.Task, t.ID)
			}
			if err != nil {
				return "", "", err
			}
			base = joined
		}
	}
	if base == main {
		return main, "", nil
	}
	ref := Ref(t.ID, base)
	if _, err := gitx.Run(ctx, r.Path, "update-ref", ref, base); err != nil {
		return "", "", err
	}
	return base, ref, nil
}

// Forget deletes a task's base refs: once it merged or was cancelled nothing fetches them.
func Forget(ctx context.Context, repoPath string, taskID int64) {
	out, err := gitx.Run(ctx, repoPath, "for-each-ref", "--format=%(refname)", "refs/swarm/base/")
	if err != nil {
		return
	}
	for _, ref := range strings.Fields(out) {
		if strings.HasPrefix(ref, prefix(taskID)) {
			_, _ = gitx.Run(ctx, repoPath, "update-ref", "-d", ref)
		}
	}
}
