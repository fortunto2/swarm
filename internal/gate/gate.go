// Package gate decides whether a submitted commit is done. It plays the part of the Lean
// checker: the agent's word counts for nothing, the checks count for everything.
package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/fortunto2/swarm/internal/gitx"
)

type Verdict struct {
	OK     bool
	Stage  string // "static" | "gate"
	Reason string // short, for the event log
	Tail   string // last 4K of the failing output, for the next attempt
}

const tailBytes = 4096

// Tail keeps the last n bytes of an output, marked when cut.
func Tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func tail(s string) string { return Tail(s, tailBytes) }

var testFile = regexp.MustCompile(`(^|/)(test_[^/]*\.py|[^/]*_test\.(go|py)|[^/]*\.(test|spec)\.[jt]sx?|tests?/.*)$`)

// Added lines that switch a test off. Language-agnostic on purpose: a false alarm costs a
// retry, a silent skip costs a merged lie.
var skipAdded = regexp.MustCompile(`(?m)^\+.*(\bt\.Skip(Now|f)?\(|@pytest\.mark\.(skip|xfail)|\b(it|describe|test)\.skip\(|#\[ignore\]|@unittest\.skip|@Disabled)`)

// Static checks run without a model: the change stays inside the task's files, and no test
// is deleted or switched off unless the spec allows it.
func Static(ctx context.Context, wt, base, commit string, claims []string, allowTestChanges bool) Verdict {
	files, err := gitx.ChangedFiles(ctx, wt, base, commit)
	if err != nil {
		return Verdict{Stage: "static", Reason: "diff failed", Tail: err.Error()}
	}
	if len(files) == 0 {
		return Verdict{Stage: "static", Reason: "empty change"}
	}
	var outside, deletedTests []string
	for f, st := range files {
		if len(claims) > 0 && !slices.ContainsFunc(claims, func(c string) bool { return owns(c, f) }) {
			outside = append(outside, f)
		}
		if st == "D" && testFile.MatchString(f) {
			deletedTests = append(deletedTests, f)
		}
	}
	slices.Sort(outside)
	slices.Sort(deletedTests)
	if len(outside) > 0 {
		return Verdict{Stage: "static", Reason: "files outside the task's claims",
			Tail: "changed but not owned (claim them first): " + strings.Join(outside, ", ")}
	}
	if allowTestChanges {
		return Verdict{OK: true, Stage: "static"}
	}
	if len(deletedTests) > 0 {
		return Verdict{Stage: "static", Reason: "tests deleted", Tail: "deleted: " + strings.Join(deletedTests, ", ")}
	}
	diff, err := gitx.Run(ctx, wt, "diff", "-U0", base, commit)
	if err != nil {
		return Verdict{Stage: "static", Reason: "diff failed", Tail: err.Error()}
	}
	if m := skipAdded.FindString(diff); m != "" {
		return Verdict{Stage: "static", Reason: "a test was switched off", Tail: "added: " + strings.TrimSpace(m)}
	}
	return Verdict{OK: true, Stage: "static"}
}

// owns: a claim is a file path or a directory prefix ending in "/".
func owns(claim, file string) bool {
	if strings.HasSuffix(claim, "/") {
		return strings.HasPrefix(file, claim)
	}
	if ok, _ := filepath.Match(claim, file); ok {
		return true
	}
	return claim == file
}

// Run executes the repo's gate commands in the worktree, in order, under one timeout.
// The process group is killed on timeout so no test runner outlives the gate.
func Run(ctx context.Context, wt string, cmds []string, timeout time.Duration) Verdict {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, c := range cmds {
		var out bytes.Buffer
		cmd := exec.Command("sh", "-c", c)
		cmd.Dir = wt
		cmd.Stdout, cmd.Stderr = &out, &out
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return Verdict{Stage: "gate", Reason: c + ": " + err.Error()}
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				var ee *exec.ExitError
				code := -1
				if errors.As(err, &ee) {
					code = ee.ExitCode()
				}
				return Verdict{Stage: "gate", Reason: fmt.Sprintf("%s: exit %d", c, code), Tail: tail(out.String())}
			}
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
			return Verdict{Stage: "gate", Reason: c + ": timeout after " + timeout.String(), Tail: tail(out.String())}
		}
	}
	return Verdict{OK: true, Stage: "gate"}
}
