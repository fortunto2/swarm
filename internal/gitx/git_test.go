package gitx

import (
	"context"
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
