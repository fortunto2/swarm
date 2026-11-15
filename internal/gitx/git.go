// Package gitx runs the git CLI. Worktrees are simpler through the CLI than through go-git.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/fortunto2/swarm/internal/envx"
)

// hardened: the daemon's git runs on repositories a worker can write to (a worktree shares
// the main .git), so nothing configured there may execute: no hooks, no fsmonitor, no LFS
// filters, no network transport. -c on the command line overrides the repo's config.
var hardened = []string{
	"--literal-pathspecs", // a file named [a]_test.go or :(exclude)x is a path, not a pattern
	"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null",
	"-c", "diff.external=", "-c", "core.pager=cat",
	"-c", "filter.lfs.smudge=", "-c", "filter.lfs.process=", "-c", "filter.lfs.required=false",
	"-c", "protocol.allow=never",
}

// Run executes git in dir and returns trimmed stdout; stderr goes into the error.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	full := append(append(append([]string{}, hardened...), "-C", dir), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	// LC_ALL=C: messages are parsed. The committer is swarm: the daemon commits only by
	// rebasing, and must not depend on the operator's git identity being configured.
	cmd.Env = append(envx.Clean(), "GIT_LFS_SKIP_SMUDGE=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_COMMITTER_NAME=swarm", "GIT_COMMITTER_EMAIL=swarm@local")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// ErrConflict: Join found the two commits conflict.
var ErrConflict = errors.New("conflict")

// Join makes a merge commit of a and b without a worktree (git merge-tree, git 2.38+). It is
// deterministic: the identity and both dates are fixed (b's committer date), so the same two
// parents always give the same commit id.
func Join(ctx context.Context, dir, a, b, msg string) (string, error) {
	tree, err := Run(ctx, dir, "merge-tree", "--write-tree", "--no-messages", a, b)
	if err != nil {
		// exit 1 is a conflict; 128 (unrelated histories) and 129 (git older than 2.38) are not
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", ErrConflict
		}
		return "", err
	}
	tree, _, _ = strings.Cut(tree, "\n")
	when, err := Run(ctx, dir, "show", "-s", "--format=%ct", b)
	if err != nil {
		return "", err
	}
	full := append(append(append([]string{}, hardened...), "-C", dir), "commit-tree", tree, "-p", a, "-p", b, "-m", msg)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(envx.Clean(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=swarm", "GIT_AUTHOR_EMAIL=swarm@local", "GIT_COMMITTER_NAME=swarm", "GIT_COMMITTER_EMAIL=swarm@local",
		"GIT_AUTHOR_DATE=@"+when+" +0000", "GIT_COMMITTER_DATE=@"+when+" +0000")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git commit-tree: %w: %s", err, strings.TrimSpace(errb.String()))
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

// ChangedFiles: what `to` adds over its merge-base with `upstream` (git diff a...b), with
// status letters (A, M, D). Three dots on purpose: a branch's own change, whether or not it
// was rebased, branched from an older main, or merged main in.
func ChangedFiles(ctx context.Context, dir, upstream, to string) (map[string]string, error) {
	// -z: paths come raw, NUL-separated; without it git quotes non-ASCII names and a
	// deleted "tests/é_test.go" would match nothing.
	out, err := Run(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", "--name-status", "--no-renames", "-z", upstream+"..."+to)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	f := strings.Split(strings.Trim(out, "\x00"), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		m[f[i+1]] = f[i]
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

// Transfer is Run for the fetches and pushes a node makes to its coordinator's copy: the
// same hardening, with the file, ssh and https transports allowed (never ext::, which runs
// a command), and the operator's own environment, so ssh finds its agent and its config.
// The URL comes from the node's profile, not from a repo a worker can write.
func Transfer(ctx context.Context, dir string, args ...string) (string, error) {
	// url.<x>.insteadOf in the repo's own config rewrites even a URL given on the command
	// line, and a worker can write that config (and what it includes, and a worktree's
	// config.worktree): refuse rather than push where it points. The operator's global and
	// system rules are theirs.
	if rw, _ := Run(ctx, dir, "config", "--show-scope", "--includes", "--get-regexp", `^url\.`); rw != "" {
		for _, ln := range strings.Split(rw, "\n") {
			if f := strings.Fields(ln); len(f) >= 2 && (f[0] == "local" || f[0] == "worktree") {
				return "", fmt.Errorf("the repo's config rewrites URLs (%s): not transferring", f[1])
			}
		}
	}
	full := append([]string{}, hardened...)
	full = append(full, "-c", "protocol.file.allow=always", "-c", "protocol.ssh.allow=always", "-c", "protocol.https.allow=always",
		// a worker can write the shared .git/config: none of what it says there runs here
		"-c", "core.sshCommand=ssh", "-c", "credential.helper=", "-c", "core.gitProxy=", "-c", "http.proxy=",
		"-c", "remote.pushDefault=", "-c", "uploadpack.packObjectsHook=", "-C", dir)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_LFS_SKIP_SMUDGE=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
