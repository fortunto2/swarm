package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fortunto2/swarm/internal/gitx"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", file)
	return git(t, dir, "rev-parse", "HEAD")
}

// A retried task's worktree is brought up to main: its commits rebased on top, or, with
// none, moved to main; a conflict or an edit leaves it for the worker.
func TestRefresh(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (repo, root, base string) {
		repo = filepath.Join(t.TempDir(), "app")
		_ = os.MkdirAll(repo, 0o755)
		git(t, repo, "init", "-q", "-b", "main")
		base = commit(t, repo, "a.txt", "a\n")
		return repo, t.TempDir(), base
	}
	t.Run("commits are rebased onto the new main", func(t *testing.T) {
		repo, root, base := setup(t)
		wt, _, err := Ensure(ctx, repo, root, "app", 1, base)
		if err != nil {
			t.Fatal(err)
		}
		commit(t, wt.Path, "w.txt", "work\n")
		main := commit(t, repo, "m.txt", "fixed on main\n")
		moved, err := Refresh(ctx, wt.Path, main)
		if err != nil || !moved {
			t.Fatalf("moved=%v err=%v", moved, err)
		}
		if !gitx.IsAncestor(ctx, wt.Path, main, "HEAD") {
			t.Fatal("HEAD does not contain the new main")
		}
		for _, f := range []string{"w.txt", "m.txt"} {
			if _, err := os.Stat(filepath.Join(wt.Path, f)); err != nil {
				t.Fatalf("%s missing after the rebase", f)
			}
		}
	})
	t.Run("nothing committed: moved to main", func(t *testing.T) {
		repo, root, base := setup(t)
		wt, _, _ := Ensure(ctx, repo, root, "app", 2, base)
		main := commit(t, repo, "m.txt", "m\n")
		if moved, err := Refresh(ctx, wt.Path, main); err != nil || !moved || git(t, wt.Path, "rev-parse", "HEAD") != main {
			t.Fatalf("moved=%v err=%v", moved, err)
		}
	})
	t.Run("a conflict is undone and left to the worker", func(t *testing.T) {
		repo, root, base := setup(t)
		wt, _, _ := Ensure(ctx, repo, root, "app", 3, base)
		mine := commit(t, wt.Path, "a.txt", "mine\n")
		main := commit(t, repo, "a.txt", "theirs\n")
		moved, err := Refresh(ctx, wt.Path, main)
		if err != nil || moved || git(t, wt.Path, "rev-parse", "HEAD") != mine {
			t.Fatalf("moved=%v err=%v", moved, err)
		}
		if out := git(t, wt.Path, "status", "--porcelain"); out != "" {
			t.Fatalf("left mid-rebase: %s", out)
		}
	})
	t.Run("uncommitted edits are not moved", func(t *testing.T) {
		repo, root, base := setup(t)
		wt, _, _ := Ensure(ctx, repo, root, "app", 4, base)
		_ = os.WriteFile(filepath.Join(wt.Path, "draft.txt"), []byte("x"), 0o644)
		main := commit(t, repo, "m.txt", "m\n")
		if moved, err := Refresh(ctx, wt.Path, main); err != nil || moved || git(t, wt.Path, "rev-parse", "HEAD") != base {
			t.Fatalf("moved=%v err=%v", moved, err)
		}
	})
}
