package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A worker can write the shared .git/config; url.<x>.insteadOf there rewrites even a URL
// given on the command line, so a node's transfer refuses instead of going where it points.
func TestTransferRefusesARepoThatRewritesURLs(t *testing.T) {
	dir := t.TempDir()
	good, evil := filepath.Join(dir, "good.git"), filepath.Join(dir, "evil.git")
	repo := filepath.Join(dir, "repo")
	for _, args := range [][]string{{"init", "-q", "--bare", good}, {"init", "-q", "--bare", evil}, {"init", "-q", repo}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	ctx := context.Background()
	if _, err := Transfer(ctx, repo, "ls-remote", good); err != nil {
		t.Fatalf("a clean repo cannot reach its remote: %v", err)
	}
	if out, err := exec.Command("git", "-C", repo, "config", "url."+evil+".insteadOf", good).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := Transfer(ctx, repo, "ls-remote", good); err == nil || !strings.Contains(err.Error(), "rewrites URLs") {
		t.Fatalf("a rewritten URL was followed: %v", err)
	}
}

// The same rule from a file the repo's config includes (a worker controls both) is refused
// too; a global rule (the operator's own) is not.
func TestTransferRefusesARewriteIncludedByTheRepo(t *testing.T) {
	dir := t.TempDir()
	good, evil, repo := filepath.Join(dir, "good.git"), filepath.Join(dir, "evil.git"), filepath.Join(dir, "repo")
	for _, args := range [][]string{{"init", "-q", "--bare", good}, {"init", "-q", "--bare", evil}, {"init", "-q", repo}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	inc := filepath.Join(dir, "planted.cfg")
	if err := os.WriteFile(inc, []byte("[url \""+evil+"\"]\n\tinsteadOf = "+good+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "config", "include.path", inc).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := Transfer(context.Background(), repo, "ls-remote", good); err == nil || !strings.Contains(err.Error(), "rewrites URLs") {
		t.Fatalf("a rewrite in an included file was followed: %v", err)
	}
}

func gitOK(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// Live (2026-11-15): `git branch -f swarm/T7 <commit>` was refused because the task's own
// worktree had swarm/T7 checked out, and the merge blocked. The task's own worktree is
// the one holder that may be passed over; any other holder is refused by name.
func TestSetBranchMovesABranchHeldByTheOwnWorktreeOnly(t *testing.T) {
	dir := t.TempDir()
	repo, own, other := filepath.Join(dir, "repo"), filepath.Join(dir, "own"), filepath.Join(dir, "other")
	gitOK(t, dir, "init", "-q", "-b", "main", repo)
	gitOK(t, repo, "commit", "-q", "--allow-empty", "-m", "one")
	gitOK(t, repo, "worktree", "add", "-q", "-b", "mine", own)
	gitOK(t, repo, "worktree", "add", "-q", "-b", "theirs", other)
	gitOK(t, own, "commit", "-q", "--allow-empty", "-m", "two")
	ctx := context.Background()
	want, err := Head(ctx, own, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetBranch(ctx, repo, "mine", want, own); err != nil {
		t.Fatalf("own worktree's branch not moved: %v", err)
	}
	if err := SetBranch(ctx, repo, "theirs", want, own); err == nil || !strings.Contains(err.Error(), "not this task's") {
		t.Fatalf("a branch held by another worktree was moved or the error is unclear: %v", err)
	}
	if got, _ := Head(ctx, repo, "refs/heads/theirs"); got == want {
		t.Fatal("the other worktree's branch moved")
	}
	if err := SetBranch(ctx, repo, "free", want, own); err != nil {
		t.Fatalf("a free branch: %v", err)
	}
	if got, _ := Head(ctx, repo, "refs/heads/free"); got != want {
		t.Fatalf("free branch at %s, want %s", got, want)
	}
}
