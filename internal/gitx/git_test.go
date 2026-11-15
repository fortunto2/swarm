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
