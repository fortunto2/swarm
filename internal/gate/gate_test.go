package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	v := Run(context.Background(), d, []string{"sleep 30 & sleep 30"}, 300*time.Millisecond)
	if v.OK || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout not enforced: ok=%v after %v (%s)", v.OK, time.Since(start), v.Reason)
	}
}

func TestRunStopsAtFirstFailure(t *testing.T) {
	d := t.TempDir()
	v := Run(context.Background(), d, []string{"true", "echo boom; exit 3", "touch never"}, time.Minute)
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
