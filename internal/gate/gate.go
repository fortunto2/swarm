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

	"github.com/fortunto2/swarm/internal/envx"
	"github.com/fortunto2/swarm/internal/gitx"
)

type Verdict struct {
	OK     bool
	Stage  string   // "static" | "gate"
	Reason string   // short, for the event log
	Tail   string   // last 4K of the failing output, for the next attempt
	Flaky  []string // commands that failed, then passed on a rerun
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

// testFile: paths that hold tests, across the stacks swarm meets (Go, Python, JS/TS, Rust,
// Swift, Kotlin, Java, Ruby).
var testFile = regexp.MustCompile(`(^|/)(` +
	`test_[^/]*\.py|[^/]*_test\.(go|py)|` + // Go, Python
	`[^/]*\.(test|spec)\.[cm]?[jt]sx?|__tests__/.*|` + // JS/TS
	`[^/]*Tests?\.(java|kt|swift)|` + // JUnit, Kotlin, XCTest
	`[^/]*_spec\.rb|` + // RSpec
	`tests?/.*|spec/.*)$`)

// Added lines that switch a test off or narrow a run to a few tests. Language-agnostic on
// purpose: a false alarm costs a retry, a silent skip costs a merged lie.
var skipAdded = regexp.MustCompile(`(?m)^\+.*(` +
	`\.Skip(Now|f)?\(|^\+//go:build ignore|` + // Go: t/b/tb/s.T().Skip, build-ignored test files
	`@pytest\.mark\.(skip|xfail)|pytest\.skip\(|\.skipTest\(|@unittest\.skip|` + // Python
	`\b(it|describe|test)\.(skip|only|todo)\(|` + // JS/TS
	`#\[ignore\]|@Disabled|@Ignore\b|XCTSkip)`) // Rust, JUnit 5/4, XCTest

// xit( / fit( / fdescribe( as a call of their own (start of line or after a space, ; or
// paren), never as a method: `model.fit(` is not a focused test.
var focusAdded = regexp.MustCompile(`(?m)^\+(?:.*[\s;(])?(xit|xdescribe|xtest|fit|fdescribe)\(`)

// switchedOff returns the first added line that switches a test off or narrows a run.
func switchedOff(diff string) string {
	if m := skipAdded.FindString(diff); m != "" {
		return m
	}
	return focusAdded.FindString(diff)
}

// AllowTestsMarker on a line of its own in a task's spec lets it delete or skip tests (a
// task that rewrites a suite). The spec is not the worker's to edit, and a sentence that
// merely mentions the marker does not count.
const AllowTestsMarker = "[allow-test-changes]"

var allowTests = regexp.MustCompile(`(?m)^\s*\[allow-test-changes\]\s*$`)

// AllowsTestChanges: the spec grants the permission explicitly.
func AllowsTestChanges(spec string) bool { return allowTests.MatchString(spec) }

// Static checks run without a model: the change stays inside the task's files, and no test
// is deleted or switched off unless the spec allows it. The change is measured against
// main as it is now (three-dot: from the merge-base), so it is the task's own change only.
func Static(ctx context.Context, wt, main, commit string, claims []string, allowTestChanges bool) Verdict {
	files, err := gitx.ChangedFiles(ctx, wt, main, commit)
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
		if (st == "D" || st == "T") && testFile.MatchString(f) { // T: replaced by a symlink
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
	// only test files are searched for switched-off tests: `model.fit(` in app code is fine
	var tests []string
	for f := range files {
		if testFile.MatchString(f) {
			tests = append(tests, f)
		}
	}
	if len(tests) == 0 {
		return Verdict{OK: true, Stage: "static"}
	}
	slices.Sort(tests)
	diff, err := gitx.Run(ctx, wt, append([]string{"diff", "-U0", main + "..." + commit, "--"}, tests...)...)
	if err != nil {
		return Verdict{Stage: "static", Reason: "diff failed", Tail: err.Error()}
	}
	if m := switchedOff(diff); m != "" {
		return Verdict{Stage: "static", Reason: "a test was switched off", Tail: "added: " + strings.TrimSpace(m)}
	}
	return Verdict{OK: true, Stage: "static"}
}

// Overlap: two claims can touch the same file. A claim is a path, a glob, or a directory
// prefix ending in "/".
func Overlap(a, b string) bool {
	if a == b || owns(a, b) || owns(b, a) {
		return true
	}
	return strings.HasSuffix(a, "/") && strings.HasSuffix(b, "/") && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a))
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
// A command that fails is run again up to `reruns` times (flaky tests, PRD §12); one that
// passes on a rerun passes, and is named in Flaky so a person can quarantine the test.
// The process group is killed on timeout so no test runner outlives the gate.
func Run(ctx context.Context, wt string, cmds []string, timeout time.Duration, reruns int) Verdict {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var flaky []string
	for _, c := range cmds {
		v := runOne(ctx, wt, c, timeout)
		for i := 0; !v.OK && i < reruns && ctx.Err() == nil; i++ {
			if v = runOne(ctx, wt, c, timeout); v.OK {
				flaky = append(flaky, c)
			}
		}
		if !v.OK {
			v.Flaky = flaky
			return v
		}
	}
	return Verdict{OK: true, Stage: "gate", Flaky: flaky}
}

func runOne(ctx context.Context, wt, c string, timeout time.Duration) Verdict {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "sh", "-c", c)
	cmd.Dir = wt
	cmd.Env = envx.Clean() // the gate runs the worker's code: it gets no credentials either
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// A child that left the group (setsid) can hold the output pipe forever: stop waiting
	// for it after this, or one stray daemon in a test wedges the gate queue.
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return Verdict{Stage: "gate", Reason: c + ": timeout after " + timeout.String(), Tail: tail(out.String())}
	}
	if err != nil {
		var ee *exec.ExitError
		code := -1
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return Verdict{Stage: "gate", Reason: fmt.Sprintf("%s: exit %d", c, code), Tail: tail(out.String())}
	}
	return Verdict{OK: true, Stage: "gate"}
}
