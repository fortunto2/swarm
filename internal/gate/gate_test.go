package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func repo(t *testing.T) (string, string) {
	t.Helper()
	d := t.TempDir()
	git(t, d, "init", "-q", "-b", "main")
	git(t, d, "config", "user.email", "t@example.com")
	git(t, d, "config", "user.name", "t")
	write(t, d, "a.go", "package a\n")
	write(t, d, "a_test.go", "package a\nfunc TestA(t *T) {}\n")
	git(t, d, "add", "-A")
	git(t, d, "commit", "-qm", "base")
	return d, head(t, d)
}

func write(t *testing.T, d, f, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(d, f)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, f), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func head(t *testing.T, d string) string {
	out := git(t, d, "rev-parse", "HEAD")
	return out[:len(out)-1]
}

func commit(t *testing.T, d string) string {
	git(t, d, "add", "-A")
	git(t, d, "commit", "-qm", "c")
	return head(t, d)
}

func TestStatic(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		change func(d string)
		claims []string
		ok     bool
	}{
		{"inside claims", func(d string) { write(t, d, "a.go", "package a\n// x\n") }, []string{"a.go"}, true},
		{"no claims = anything", func(d string) { write(t, d, "b.go", "package a\n") }, nil, true},
		{"dir claim", func(d string) { write(t, d, "pkg/x.go", "package pkg\n") }, []string{"pkg/"}, true},
		{"outside claims", func(d string) { write(t, d, "b.go", "package a\n") }, []string{"a.go"}, false},
		{"test deleted", func(d string) { os.Remove(filepath.Join(d, "a_test.go")) }, nil, false},
		{"test skipped", func(d string) {
			write(t, d, "a_test.go", "package a\nfunc TestA(t *T) { t.Skip(\"later\") }\n")
		}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, base := repo(t)
			c.change(d)
			v := Static(ctx, d, base, commit(t, d), c.claims, false)
			if v.OK != c.ok {
				t.Fatalf("ok=%v want %v: %s %s", v.OK, c.ok, v.Reason, v.Tail)
			}
		})
	}
}

func TestRunTimeoutKillsGroup(t *testing.T) {
	d := t.TempDir()
	start := time.Now()
	v := Run(context.Background(), d, []string{"sleep 30 & sleep 30"}, 300*time.Millisecond, 0)
	if v.OK || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout not enforced: ok=%v after %v (%s)", v.OK, time.Since(start), v.Reason)
	}
}

func TestRunStopsAtFirstFailure(t *testing.T) {
	d := t.TempDir()
	v := Run(context.Background(), d, []string{"true", "echo boom; exit 3", "touch never"}, time.Minute, 1)
	if v.OK || v.Reason != "echo boom; exit 3: exit 3" || v.Tail != "boom\n" {
		t.Fatalf("got %+v", v)
	}
	if _, err := os.Stat(filepath.Join(d, "never")); err == nil {
		t.Fatal("ran a command after a failure")
	}
}

func TestOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"a.go", "a.go", true}, {"a.go", "b.go", false},
		{"pkg/", "pkg/x.go", true}, {"pkg/x.go", "pkg/", true},
		{"pkg/", "pkg/sub/", true}, {"pkg/", "pkgx/", false},
		{"*.go", "a.go", true}, {"docs/", "a.go", false},
	}
	for _, c := range cases {
		if got := Overlap(c.a, c.b); got != c.want {
			t.Errorf("Overlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// A command that fails once and then passes is flaky: the gate passes and names it. One
// that fails every time still fails after its reruns.
func TestRunRerunsFlakyOnce(t *testing.T) {
	d := t.TempDir()
	flaky := "if [ -f once ]; then exit 0; fi; touch once; exit 1"
	v := Run(context.Background(), d, []string{flaky}, time.Minute, 1)
	if !v.OK || len(v.Flaky) != 1 || v.Flaky[0] != flaky {
		t.Fatalf("flaky command: %+v", v)
	}
	d2 := t.TempDir()
	if v := Run(context.Background(), d2, []string{"echo x >> runs; exit 1"}, time.Minute, 1); v.OK {
		t.Fatal("a command failing every time passed")
	}
	if runs, _ := os.ReadFile(filepath.Join(d2, "runs")); string(runs) != "x\nx\n" {
		t.Fatalf("want the failing command run twice (once + one rerun), got %q", runs)
	}
}

// Each way of switching a test off, in a test file, is caught; the same words in app code
// or in a sentence are not.
func TestSkipDetection(t *testing.T) {
	caught := []string{
		"t.Skip(\"later\")", "tb.Skip()", "s.T().Skip()", "b.SkipNow()",
		"@pytest.mark.skip", "pytest.skip(\"x\")", "self.skipTest(\"x\")", "@unittest.skip",
		"it.skip('x', () => {})", "describe.only('x', () => {})", "xit('x', () => {})", "fit('x', () => {})",
		"#[ignore]", "@Disabled", "@Ignore", "try XCTSkipIf(true)",
	}
	for _, line := range caught {
		if switchedOff("+"+line) == "" {
			t.Errorf("not caught: %s", line)
		}
	}
	for _, line := range []string{"model.fit(x)", "pending := 3", "return it.done()"} {
		if switchedOff("+"+line) != "" {
			t.Errorf("false alarm: %s", line)
		}
	}
	for _, f := range []string{"FooTest.java", "src/BarTests.swift", "a/b_spec.rb", "x.test.mjs", "__tests__/a.js", "pkg/a_test.go"} {
		if !testFile.MatchString(f) {
			t.Errorf("not a test file: %s", f)
		}
	}
	if AllowsTestChanges("do not use [allow-test-changes] here") || !AllowsTestChanges("rewrite the suite\n[allow-test-changes]\n") {
		t.Error("the marker must be a line of its own")
	}
}

// A non-ASCII test file deleted is still a deleted test (git quotes such paths without -z).
func TestDeletedNonASCIITest(t *testing.T) {
	ctx := context.Background()
	d, _ := repo(t)
	write(t, d, "tests/é_test.go", "package a\n")
	base := commit(t, d)
	os.Remove(filepath.Join(d, "tests/é_test.go"))
	v := Static(ctx, d, base, commit(t, d), nil, false)
	if v.OK || v.Reason != "tests deleted" {
		t.Fatalf("got %+v", v)
	}
}

// The gate gives the code it runs no credentials.
func TestGateEnvHasNoCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("GH_TOKEN", "gh-test")
	d := t.TempDir()
	if v := Run(context.Background(), d, []string{`env > seen`}, time.Minute, 0); !v.OK {
		t.Fatal(v.Reason)
	}
	seen, _ := os.ReadFile(filepath.Join(d, "seen"))
	if strings.Contains(string(seen), "sk-test") || strings.Contains(string(seen), "gh-test") {
		t.Fatal("a credential reached the gate's processes")
	}
}

// A child that escapes the process group and holds the output pipe does not hang the gate.
func TestGateDoesNotHangOnEscapedChild(t *testing.T) {
	d := t.TempDir()
	start := time.Now()
	v := Run(context.Background(), d, []string{`perl -MPOSIX -e 'POSIX::setsid(); sleep 60' & sleep 60`}, 300*time.Millisecond, 0)
	if v.OK || time.Since(start) > 15*time.Second {
		t.Fatalf("gate hung or passed: ok=%v after %v", v.OK, time.Since(start))
	}
}
