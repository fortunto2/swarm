package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fortunto2/swarm/internal/gate"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@e"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// change: a repo whose HEAD adds two.go over base.
func change(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "one.go"), []byte("package app\n"), 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "base")
	base = git(t, dir, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "two.go"), []byte("package app\n\nfunc Two() int { return 2 }\n"), 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "T1: add Two")
	return dir, base
}

type fake struct {
	calls  int
	prompt string
	say    string
	err    error
}

func (f *fake) run(_ context.Context, prompt, _ string) (string, error) {
	f.calls++
	f.prompt = prompt
	return f.say, f.err
}

func spec(dir, base, cache string, f *fake) Spec {
	return Spec{Task: Task{ID: 1, Title: "add Two", Spec: "func Two() int returning 2", Acceptance: "TestTwo passes"},
		Base: base, Dir: dir, Reviewer: "rev", Run: f.run, Cache: cache}
}

func TestApproveIsRememberedForTheSameChange(t *testing.T) {
	dir, base := change(t)
	cache := t.TempDir()
	f := &fake{say: "Looks right.\nVERDICT: approve"}
	v := Gate(context.Background(), spec(dir, base, cache, f))
	if !v.OK || v.Stage != "review" || f.calls != 1 {
		t.Fatalf("approve: %+v (calls %d)", v, f.calls)
	}
	for _, want := range []string{"Task T1: add Two", "func Two() int returning 2", "TestTwo passes", "+func Two() int { return 2 }", "VERDICT: approve"} {
		if !strings.Contains(f.prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if v := Gate(context.Background(), spec(dir, base, cache, f)); !v.OK || f.calls != 1 {
		t.Fatalf("the same change was reviewed (and paid for) again: %+v, calls %d", v, f.calls)
	}
	s := spec(dir, base, cache, f)
	s.Task.Acceptance = "TestTwo and TestThree pass" // another task text: a new review
	if Gate(context.Background(), s); f.calls != 2 {
		t.Fatalf("a changed task reused an old approval: calls %d", f.calls)
	}
}

func TestRejectFailsTheGateWithTheFindings(t *testing.T) {
	dir, base := change(t)
	f := &fake{say: "two.go:3: Two returns 2, the task asked for 3 when x is odd\nVERDICT: reject"}
	v := Gate(context.Background(), spec(dir, base, t.TempDir(), f))
	if v.OK || v.Stage != "review" || !strings.Contains(v.Tail, "two.go:3: Two returns 2") || strings.Contains(v.Tail, "VERDICT") {
		t.Fatalf("reject: %+v", v)
	}
}

func TestAReviewThatCannotRunDoesNotHoldTheWork(t *testing.T) {
	dir, base := change(t)
	for name, f := range map[string]*fake{
		"error":      {err: errors.New("not logged in")},
		"no verdict": {say: "I think it is fine"},
	} {
		v := Gate(context.Background(), spec(dir, base, t.TempDir(), f))
		if !v.OK || len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], "review did not run") {
			t.Errorf("%s: %+v, want a pass with a warning", name, v)
		}
	}
}

func TestNoChangeIsNotReviewed(t *testing.T) {
	dir, _ := change(t)
	f := &fake{say: "VERDICT: reject"}
	if v := Gate(context.Background(), spec(dir, git(t, dir, "rev-parse", "HEAD"), "", f)); !v.OK || f.calls != 0 {
		t.Fatalf("empty diff: %+v calls %d", v, f.calls)
	}
}

func TestParseTakesTheLastVerdict(t *testing.T) {
	text := "The rules say to end with\nVERDICT: approve\nfor a clean change. It is not clean.\na.go:1: x\n**VERDICT: REJECT**"
	if v, f := Parse(text); v != "reject" || !strings.HasPrefix(f, "The rules say") || strings.Contains(f, "REJECT") {
		t.Fatalf("Parse = %q, %q", v, f)
	}
	if v, _ := Parse("no verdict here"); v != "" {
		t.Fatalf("no verdict read as %q", v)
	}
}

func TestAfterKeepsTheGatesWarningsAndSkipsAFailedGate(t *testing.T) {
	dir, base := change(t)
	f := &fake{say: "VERDICT: approve"}
	failed := gate.Verdict{Stage: "gate", Reason: "go test"}
	if v := After(context.Background(), failed, spec(dir, base, "", f)); v.OK || f.calls != 0 {
		t.Fatalf("a failed gate was reviewed: %+v", v)
	}
	v := After(context.Background(), gate.Verdict{OK: true, Warnings: []string{"w"}, Flaky: []string{"go test"}}, spec(dir, base, "", f))
	if !v.OK || len(v.Warnings) != 1 || v.Warnings[0] != "w" || len(v.Flaky) != 1 {
		t.Fatalf("after: %+v", v)
	}
}
