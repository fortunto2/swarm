// Package worktree gives each task its own checkout on its own branch, outside the repo.
package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fortunto2/swarm/internal/gitx"
)

type WT struct {
	Path   string
	Branch string
}

func Branch(taskID int64) string { return fmt.Sprintf("swarm/T%d", taskID) }

// Ensure creates the worktree for a task from base, or reuses it on a later attempt:
// a retry continues from what the previous attempt committed.
func Ensure(ctx context.Context, repo, root, repoName string, taskID int64, base string) (WT, error) {
	wt := WT{Path: filepath.Join(root, fmt.Sprintf("%s--T%d", repoName, taskID)), Branch: Branch(taskID)}
	if _, err := os.Stat(filepath.Join(wt.Path, ".git")); err == nil {
		return wt, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return wt, err
	}
	args := []string{"worktree", "add", "-b", wt.Branch, wt.Path, base}
	if _, err := gitx.Head(ctx, repo, "refs/heads/"+wt.Branch); err == nil {
		args = []string{"worktree", "add", wt.Path, wt.Branch} // branch survived a removed worktree
	}
	_, err := gitx.Run(ctx, repo, args...)
	return wt, err
}

// Remove deletes the worktree and its branch after merged/cancelled.
func Remove(ctx context.Context, repo string, wt WT) error {
	if _, err := gitx.Run(ctx, repo, "worktree", "remove", "--force", wt.Path); err != nil && !os.IsNotExist(statErr(wt.Path)) {
		return err
	}
	_, err := gitx.Run(ctx, repo, "branch", "-D", wt.Branch)
	return err
}

func statErr(p string) error { _, err := os.Stat(p); return err }
