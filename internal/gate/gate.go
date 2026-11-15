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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fortunto2/swarm/internal/envx"
	"github.com/fortunto2/swarm/internal/gitx"
)

type Verdict struct {
	OK       bool
	Stage    string   // "static" | "gate"
	Reason   string   // short, for the event log
	Tail     string   // last 4K of the failing output, for the next attempt
	Flaky    []string // commands that failed, then passed on a rerun
	Warnings []string // trusted profiles: what strict would have failed, for a person to read
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

// testFile: paths that hold tests or decide which tests run, across the stacks swarm meets
// (Go, Python, JS/TS, Rust, Swift, Kotlin, Java, Ruby). Case-insensitive: SwiftPM's Tests/.
var testFile = regexp.MustCompile(`(?i)(^|/)(` +
	`test_[^/]*\.py|[^/]*_test\.(go|py)|conftest\.py|pytest\.ini|` + // Go, Python
	`[^/]*\.(test|spec)\.[cm]?[jt]sx?|__tests__/.*|(jest|vitest|karma)\.config\.[^/]*|` + // JS/TS
	`[^/]*(Tests?|Spec)\.(java|kt|swift)|__snapshots__/.*|[^/]*\.snap$|` + // JUnit, Kotlin, XCTest, snapshots
	`[^/]*_spec\.rb|` + // RSpec
	`tests?/.*|spec/.*|e2e/.*|cypress/.*)$`)

// Added lines that switch a test off or narrow a run to a few tests. Language-agnostic on
// purpose: a false alarm costs a retry, a silent skip costs a merged lie.
var skipAdded = regexp.MustCompile(`(?m)^\+.*(` +
	`\.Skip(Now|f)?\(|//go:build\s+ignore|` + // Go: t/b/tb/s.T().Skip, build-ignored test files
	`@pytest\.mark\.(skip|xfail)|pytest\.skip\(|\.skipTest\(|@unittest\.skip|` + // Python
	`\b(it|describe|test)\.(skip|only|todo)\(|` + // JS/TS
	`#\[ignore\]|@Disabled|@Ignore\b|XCTSkip|` + // Rust, JUnit 5/4, XCTest
	`func TestMain\(|os\.Exit\(|pytest_collection_modifyitems|collect_ignore)`) // test runners taken over

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

// AllowProtectedMarker on a line of its own lets a task change protected paths.
const AllowProtectedMarker = "[allow-protected]"

var allowProtected = regexp.MustCompile(`(?m)^\s*\[allow-protected\]\s*$`)

// Grants: what a task's spec explicitly allows beyond its claims, and the profile's level.
// Strict makes protected paths and shrinking tests fail; trusted reports them as warnings.
type Grants struct{ Tests, Protected, Strict bool }

func GrantsFrom(spec string) Grants {
	return Grants{Tests: AllowsTestChanges(spec), Protected: allowProtected.MatchString(spec)}
}

// protected: paths that configure agents, CI or git itself. A change there runs in every
// later worker, in CI and in the operator's own sessions, so it needs an explicit grant
// even when claimed.
var protected = regexp.MustCompile(`(?i)(^|/)(` +
	`\.claude/|\.mcp\.json$|CLAUDE(\.local)?\.md$|AGENTS(\.override)?\.md$|GEMINI\.md$|\.cursor/|\.codex/|` + // agents
	`\.github/|\.gitlab-ci|\.circleci/|Jenkinsfile$|azure-pipelines\.ya?ml$|` + // CI
	`\.gitattributes$|\.gitmodules$|\.lfsconfig$|\.git/|\.githooks/|\.husky/|\.pre-commit-config|lefthook\.ya?ml$|` + // git
	`\.envrc$|\.cargo/config|\.npmrc$|\.yarnrc|\.mise\.toml$|\.tool-versions$|` + // run in the operator's shell or toolchain
	`\.vscode/|\.devcontainer/|\.idea/)`)

// Static checks run without a model: the change stays inside the task's files, and no test
// is deleted or switched off unless the spec allows it. The change is measured against
// main as it is now (three-dot: from the merge-base), so it is the task's own change only.
func Static(ctx context.Context, wt, main, commit string, claims []string, g Grants) Verdict {
	files, err := gitx.ChangedFiles(ctx, wt, main, commit)
	if err != nil {
		return Verdict{Stage: "static", Reason: "diff failed", Tail: err.Error()}
	}
	if len(files) == 0 {
		return Verdict{Stage: "static", Reason: "empty change"}
	}
	var outside, guarded, deletedTests, tests []string
	for f, st := range files {
		if !slices.ContainsFunc(claims, func(c string) bool { return owns(c, f) }) {
			outside = append(outside, f)
		}
		if protected.MatchString(f) {
			guarded = append(guarded, f)
		}
		if testFile.MatchString(f) {
			tests = append(tests, f)
			if st == "D" || st == "T" { // T: replaced by a symlink
				deletedTests = append(deletedTests, f)
			}
		}
	}
	for _, l := range [][]string{outside, guarded, deletedTests, tests} {
		slices.Sort(l)
	}
	if len(outside) > 0 {
		return Verdict{Stage: "static", Reason: "files outside the task's claims",
			Tail: "changed but not owned (claim them first): " + strings.Join(outside, ", ")}
	}
	var warn []string
	if len(guarded) > 0 && !g.Protected {
		msg := "these configure agents, CI or git and need [allow-protected] in the spec: " + strings.Join(guarded, ", ")
		if g.Strict {
			return Verdict{Stage: "static", Reason: "protected paths changed", Tail: msg}
		}
		warn = append(warn, msg)
	}
	if g.Tests || len(tests) == 0 {
		return Verdict{OK: true, Stage: "static", Warnings: warn}
	}
	if len(deletedTests) > 0 {
		return Verdict{Stage: "static", Reason: "tests deleted", Tail: "deleted: " + strings.Join(deletedTests, ", ")}
	}
	// only test files are searched: `model.fit(` or os.Exit in app code is fine
	diff, err := gitx.Run(ctx, wt, append([]string{"diff", "--no-ext-diff", "--no-textconv", "-U0", main + "..." + commit, "--"}, tests...)...)
	if err != nil {
		return Verdict{Stage: "static", Reason: "diff failed", Tail: err.Error()}
	}
	if m := switchedOff(diff); m != "" {
		return Verdict{Stage: "static", Reason: "a test was switched off", Tail: "added: " + strings.TrimSpace(m)}
	}
	if shrunk := shrunkTests(ctx, wt, main, commit, files, tests); len(shrunk) > 0 {
		msg := "more lines removed than added (assertions dropped?): " + strings.Join(shrunk, ", ")
		if g.Strict {
			return Verdict{Stage: "static", Reason: "existing tests shrank", Tail: msg}
		}
		warn = append(warn, msg)
	}
	return Verdict{OK: true, Stage: "static", Warnings: warn}
}

// shrunkTests: existing test files (modified, not new) with more lines removed than added.
// A rewrite that legitimately shortens a suite says so with [allow-test-changes].
func shrunkTests(ctx context.Context, wt, main, commit string, files map[string]string, tests []string) []string {
	out, err := gitx.Run(ctx, wt, append([]string{"diff", "--no-ext-diff", "--no-textconv", "--numstat", "-z", main + "..." + commit, "--"}, tests...)...)
	if err != nil {
		return nil
	}
	var shrunk []string
	for _, rec := range strings.Split(strings.Trim(out, "\x00"), "\x00") {
		f := strings.SplitN(rec, "\t", 3)
		if len(f) != 3 || files[f[2]] != "M" {
			continue
		}
		added, _ := strconv.Atoi(f[0])
		deleted, _ := strconv.Atoi(f[1])
		if deleted > added {
			shrunk = append(shrunk, f[2])
		}
	}
	return shrunk
}

// Overlap: two claims can touch the same file. A claim is a path, a glob, or a directory
// prefix ending in "/".
func Overlap(a, b string) bool {
	if a == "**" || b == "**" || a == b || owns(a, b) || owns(b, a) {
		return true
	}
	return strings.HasSuffix(a, "/") && strings.HasSuffix(b, "/") && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a))
}

// owns: a claim is a file path, a glob, a directory prefix ending in "/", or "**" for the
// whole repository (an explicit choice, never a default).
func owns(claim, file string) bool {
	if claim == "**" {
		return true
	}
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
	cmd.Env = envx.Minimal() // the gate runs the worker's code: it gets no credentials either
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
