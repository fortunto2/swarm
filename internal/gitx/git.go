// Package gitx runs the git CLI. Worktrees are simpler through the CLI than through go-git.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Run executes git in dir and returns trimmed stdout; stderr goes into the error.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func Head(ctx context.Context, dir, ref string) (string, error) {
	return Run(ctx, dir, "rev-parse", "--verify", ref+"^{commit}")
}

// IsAncestor: a is reachable from b.
func IsAncestor(ctx context.Context, dir, a, b string) bool {
	_, err := Run(ctx, dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// ChangedFiles between two commits, with status letters (A, M, D, R...).
func ChangedFiles(ctx context.Context, dir, from, to string) (map[string]string, error) {
	out, err := Run(ctx, dir, "diff", "--name-status", "--no-renames", from, to)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.SplitN(line, "\t", 2); len(f) == 2 {
			m[f[1]] = f[0]
		}
	}
	return m, nil
}

// CurrentBranch of a checkout; "" when HEAD is detached.
func CurrentBranch(ctx context.Context, dir string) string {
	b, err := Run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return b
}
