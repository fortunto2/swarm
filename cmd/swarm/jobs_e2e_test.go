package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A profile's job, started by hand through a running daemon: it runs in its repo's directory
// with SWARM_JOB set, its output is kept, and `swarm jobs` shows the result and the next run.
func TestJobRunByHand(t *testing.T) {
	repo := testbed(t)
	marks := t.TempDir()
	prof := profile(t, repo, "exit 1", 1, fmt.Sprintf(`
[jobs.hello]
cron = "0 9 * * 1-5"
repo = "app"
run = "pwd > %[1]s/pwd; echo \"hello from $SWARM_JOB\""

[jobs.broken]
cron = "0 9 * * 1-5"
run = "echo nope; exit 2"
`, marks))
	if out, err := swarm(t, prof, 30*time.Second, "init"); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	daemon := exec.Command(bin, "--profile", prof, "daemon")
	var dlog bytes.Buffer
	daemon.Stdout, daemon.Stderr = &dlog, &dlog
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Process.Signal(os.Interrupt); _ = daemon.Wait() })
	waitFor(t, 20*time.Second, func() bool {
		_, err := swarm(t, prof, 10*time.Second, "jobs", "run", "hello")
		return err == nil
	}, "the daemon never took the job")
	if _, err := swarm(t, prof, 10*time.Second, "jobs", "run", "broken"); err != nil {
		t.Fatal(err)
	}
	if out, err := swarm(t, prof, 10*time.Second, "jobs", "run", "nosuch"); err == nil || !strings.Contains(out, "no job") {
		t.Fatalf("an unknown job: %v %s", err, out)
	}
	var list string
	waitFor(t, 20*time.Second, func() bool {
		list, _ = swarm(t, prof, 10*time.Second, "jobs")
		return strings.Contains(list, "ok") && strings.Contains(list, "FAILED exit 2")
	}, "jobs did not finish")
	if !strings.Contains(list, "hello") || !strings.Contains(list, "next ") {
		t.Fatalf("swarm jobs:\n%s", list)
	}
	pwd, _ := os.ReadFile(filepath.Join(marks, "pwd"))
	if got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(pwd))); got != mustEval(t, repo) {
		t.Fatalf("the job ran in %q, want its repo %q", got, repo)
	}
	if st := status(t, prof); !strings.Contains(st, "jobs: 2, last run failed: broken exit 2") {
		t.Fatalf("status does not name the failing job:\n%s", st)
	}
	if tail := sqlite(t, prof, "SELECT tail FROM job_runs WHERE job='hello'"); !strings.Contains(tail, "hello from hello") {
		t.Fatalf("output kept: %q", tail)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
