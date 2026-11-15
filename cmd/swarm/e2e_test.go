package main

// End-to-end through the real binary: task add → daemon → mock worker over the board →
// gate → ff into main. No model, no quota. Phase 1 acceptance on the mock adapter.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "swarmbin")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "swarm")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func sh(t *testing.T, dir, script string) string {
	t.Helper()
	c := exec.Command("sh", "-c", script)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return strings.TrimSpace(string(out))
}

// testbed: a tiny Go module with one passing test, on main.
func testbed(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "app")
	_ = os.MkdirAll(d, 0o755)
	sh(t, d, `git init -q -b main && printf 'module app\n\ngo 1.22\n' > go.mod &&
		printf 'package app\n\nfunc One() int { return 1 }\n' > one.go &&
		printf 'package app\n\nimport "testing"\n\nfunc TestOne(t *testing.T) { if One() != 1 { t.Fatal() } }\n' > one_test.go &&
		git add -A && git commit -qm base`)
	return d
}

// profile writes swarm.toml with a mock provider running the given worker script.
func profile(t *testing.T, repo, worker string, maxAttempts int, extra ...string) string {
	t.Helper()
	p := t.TempDir()
	script := filepath.Join(p, "worker.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nset -e\n"+worker), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf(`
[profile]
name = "e2e"
worktree_root = %q

[repos.app]
path = %q
gate = ["go vet ./...", "go test ./... -count=1"]
gate_timeout = "2m"
merge = "ff"

[limits]
max_workers = 2
task_wall_clock = "1m"
max_attempts = %d

[providers.mock]
kind = "mock"
family = "mock"
slots = 2
command = [%q]
`, filepath.Join(p, "wt"), repo, maxAttempts, script) + strings.Join(extra, "\n")
	if err := os.WriteFile(filepath.Join(p, "swarm.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func swarm(t *testing.T, prof string, timeout time.Duration, args ...string) (string, error) {
	t.Helper()
	c := exec.Command(bin, append([]string{"--profile", prof}, args...)...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = c.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = c.Process.Kill()
		<-done
		t.Fatalf("swarm %v did not finish in %v\n%s", args, timeout, out)
	}
	return string(out), err
}

func addTask(t *testing.T, prof string, files string) {
	t.Helper()
	args := []string{"task", "add", "--repo", "app", "--title", "add Two", "--spec", "add func Two() int returning 2, with a test",
		"--acceptance", "go test passes; TestTwo exists"}
	if files != "" {
		args = append(args, "--files", files)
	}
	if out, err := swarm(t, prof, 30*time.Second, args...); err != nil {
		t.Fatalf("task add: %v\n%s", err, out)
	}
}

func status(t *testing.T, prof string) string {
	out, err := swarm(t, prof, 30*time.Second, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	return out
}

const goodWorker = `printf 'package app\n\nfunc Two() int { return 2 }\n' > two.go
printf 'package app\n\nimport "testing"\n\nfunc TestTwo(t *testing.T) { if Two() != 2 { t.Fatal() } }\n' > two_test.go
git add -A && git commit -qm "T$SWARM_TASK_ID: add Two" --trailer "Swarm-Task: $SWARM_TASK_ID"
swarm gate
swarm board done "$(git rev-parse HEAD)" "added Two"
`

func TestReadyToMerged(t *testing.T) {
	repo := testbed(t)
	prof := profile(t, repo, goodWorker, 4)
	before := sh(t, repo, "git rev-parse main")
	addTask(t, prof, "two.go,two_test.go")
	if out, err := swarm(t, prof, 2*time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if st := status(t, prof); !strings.Contains(st, "merged       1") {
		t.Fatalf("task not merged:\n%s", st)
	}
	after := sh(t, repo, "git rev-parse main")
	if after == before {
		t.Fatal("main did not move")
	}
	if msg := sh(t, repo, "git log -1 --format=%an%n%B main"); !strings.Contains(msg, "swarm-mock-T1") || !strings.Contains(msg, "Swarm-Task: 1") {
		t.Fatalf("commit lacks the worker identity or trailer:\n%s", msg)
	}
	sh(t, repo, "test -f two.go && go test ./... -count=1") // working tree of main updated, suite green
	if wts := sh(t, repo, "git worktree list"); strings.Count(wts, "\n") != 0 {
		t.Fatalf("worktree not cleaned:\n%s", wts)
	}
}

// A worker that switches a test off is caught by the static gate on every attempt and the
// task ends blocked; main never moves.
func TestWeakenedTestNeverMerges(t *testing.T) {
	repo := testbed(t)
	bad := `printf 'package app\n\nimport "testing"\n\nfunc TestOne(t *testing.T) { t.Skip("flaky") }\n' > one_test.go
git add -A && git commit -qm "T$SWARM_TASK_ID: quiet a test" --allow-empty
"$SWARM_BIN" board done "$(git rev-parse HEAD)"
`
	prof := profile(t, repo, bad, 2)
	before := sh(t, repo, "git rev-parse main")
	addTask(t, prof, "")
	if out, err := swarm(t, prof, 2*time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if st := status(t, prof); !strings.Contains(st, "blocked      1") {
		t.Fatalf("want blocked after 2 attempts:\n%s", st)
	}
	if sh(t, repo, "git rev-parse main") != before {
		t.Fatal("main moved on a weakened test")
	}
}

// A worker that dies without reporting is the infrastructure's failure: three of them and
// the task stops cycling.
func TestSilentWorkerBlocksAfterThreeInfraFailures(t *testing.T) {
	repo := testbed(t)
	prof := profile(t, repo, "exit 0\n", 4)
	addTask(t, prof, "")
	if out, err := swarm(t, prof, 2*time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if st := status(t, prof); !strings.Contains(st, "blocked      1") {
		t.Fatalf("want blocked:\n%s", st)
	}
}

// A file outside the claims fails the static gate.
func TestOutsideClaimsFails(t *testing.T) {
	repo := testbed(t)
	prof := profile(t, repo, goodWorker, 1)
	addTask(t, prof, "two.go") // two_test.go not claimed
	if out, err := swarm(t, prof, 2*time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if st := status(t, prof); !strings.Contains(st, "blocked      1") {
		t.Fatalf("want blocked:\n%s", st)
	}
}

// The profile's extra MCP server reaches the worker's config next to the board, and the
// after_merge hook runs with its placeholders filled (the reindex slot solograph uses).
func TestExtraMCPAndAfterMergeHook(t *testing.T) {
	repo := testbed(t)
	marks := t.TempDir()
	worker := "cp \"$SWARM_MCP_CONFIG\" " + filepath.Join(marks, "mcp.json") + "\n" + goodWorker
	prof := profile(t, repo, worker, 4, `
[mcp.codeindex]
command = "codeindex"
args = ["serve"]
roles = ["worker"]

[hooks]
after_merge = ["echo {repo} {task} > `+filepath.Join(marks, "hook")+`"]
`)
	addTask(t, prof, "")
	if out, err := swarm(t, prof, 2*time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	cfg, err := os.ReadFile(filepath.Join(marks, "mcp.json"))
	if err != nil || !strings.Contains(string(cfg), `"codeindex"`) || !strings.Contains(string(cfg), `"board"`) {
		t.Fatalf("worker mcp config lacks board or codeindex: %v\n%s", err, cfg)
	}
	hook, err := os.ReadFile(filepath.Join(marks, "hook"))
	if err != nil || strings.TrimSpace(string(hook)) != "app 1" {
		t.Fatalf("after_merge hook: %v %q", err, hook)
	}
}

// A CLI that is not logged in takes its provider out of rotation at once: no attempt is
// counted against the task, no three blind retries.
func TestNotLoggedInDisablesProvider(t *testing.T) {
	repo := testbed(t)
	prof := profile(t, repo, "echo '{\"type\":\"result\",\"is_error\":true,\"result\":\"Not logged in · Please run /login\"}'\nexit 1\n", 4)
	addTask(t, prof, "")
	out, err := swarm(t, prof, time.Minute, "daemon", "--until-idle")
	if err != nil || !strings.Contains(out, "provider disabled") {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if strings.Count(out, "worker started") != 1 {
		t.Fatalf("want exactly one start before the provider is disabled:\n%s", out)
	}
	if st := status(t, prof); !strings.Contains(st, "ready        1") {
		t.Fatalf("task should wait in ready:\n%s", st)
	}
}

// An ordinary Claude Code session reaches the swarm through `swarm mcp --profile` (no
// token): it queues a task, a worker does it, the session sees it merged.
func TestSessionMCPQueuesAndWatches(t *testing.T) {
	repo := testbed(t)
	prof := profile(t, repo, goodWorker, 4)
	if out, err := swarm(t, prof, 30*time.Second, "init"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	daemon := exec.Command(bin, "--profile", prof, "daemon")
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Process.Signal(os.Interrupt); _ = daemon.Wait() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var cs *mcp.ClientSession
	var err error
	for i := 0; i < 50; i++ { // the daemon needs a moment to open its socket
		cl := mcp.NewClient(&mcp.Implementation{Name: "test-session", Version: "0"}, nil)
		cs, err = cl.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(bin, "--profile", prof, "mcp")}, nil)
		if err == nil {
			if _, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "swarm_status"}); err == nil {
				break
			}
			cs.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("session could not reach swarm: %v", err)
	}
	defer cs.Close()
	call := func(name string, args map[string]any) string {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := res.Content[0].(*mcp.TextContent).Text
		if res.IsError {
			t.Fatalf("%s returned an error: %s", name, text)
		}
		return text
	}
	call("swarm_add_task", map[string]any{"repo": "app", "title": "add Two", "spec": "add Two()",
		"acceptance": "go test passes", "files": []string{"two.go", "two_test.go"}})
	deadline := time.Now().Add(90 * time.Second)
	for {
		if strings.Contains(call("swarm_list_tasks", map[string]any{"status": "merged"}), `"add Two"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not merged; tasks: %s", call("swarm_list_tasks", nil))
		}
		time.Sleep(300 * time.Millisecond)
	}
	if st := call("swarm_status", nil); !strings.Contains(st, `"merged":1`) {
		t.Fatalf("status: %s", st)
	}
}

// A daemon killed mid-attempt: the next one puts the task back at once (not after the
// 2-minute lease) and the restart costs the task no infra strike.
func TestRestartReconcilesRunningTasks(t *testing.T) {
	repo := testbed(t)
	marks := t.TempDir()
	marker := filepath.Join(marks, "first")
	worker := fmt.Sprintf("if [ ! -f %q ]; then touch %q; exec sleep 300; fi\n", marker, marker) + goodWorker
	prof := profile(t, repo, worker, 4)
	addTask(t, prof, "")
	first := exec.Command(bin, "--profile", prof, "daemon")
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = first.Process.Kill() // no cleanup: the worker is orphaned, the task stays running
	_ = first.Wait()
	_ = exec.Command("pkill", "-f", "sleep 300").Run()

	start := time.Now()
	if out, err := swarm(t, prof, time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("restart waited %v: the lease was not bypassed", took)
	}
	if st := status(t, prof); !strings.Contains(st, "merged       1") {
		t.Fatalf("want merged after restart:\n%s", st)
	}
}

// A merge git refuses (an untracked file in main's checkout would be overwritten) blocks
// that task with the reason and leaves the queue free; it does not retry forever.
func TestMergeFailureBlocksInsteadOfLooping(t *testing.T) {
	repo := testbed(t)
	prof := profile(t, repo, goodWorker, 4)
	if err := os.WriteFile(filepath.Join(repo, "two.go"), []byte("package app // local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addTask(t, prof, "")
	out, err := swarm(t, prof, time.Minute, "daemon", "--until-idle")
	if err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if !strings.Contains(out, "merge failed, task blocked") {
		t.Fatalf("no merge failure reported:\n%s", out)
	}
	if st := status(t, prof); !strings.Contains(st, "blocked      1") {
		t.Fatalf("want blocked:\n%s", st)
	}
}

// A dependency: B waits as draft until A merges, then runs on top of it. Each worker writes
// a file of its own, so the two tasks never touch the same path.
func TestDependencyWaitsForMerge(t *testing.T) {
	repo := testbed(t)
	worker := `f="t$SWARM_TASK_ID.go"
printf 'package app\n\nfunc T%s() int { return %s }\n' "$SWARM_TASK_ID" "$SWARM_TASK_ID" > "$f"
git add -A && git commit -qm "T$SWARM_TASK_ID"
"$SWARM_BIN" board done "$(git rev-parse HEAD)"
`
	prof := profile(t, repo, worker, 4)
	if out, err := swarm(t, prof, 30*time.Second, "task", "add", "--repo", "app", "--title", "A",
		"--spec", "a", "--acceptance", "go test passes"); err != nil || !strings.Contains(out, "task 1 ready") {
		t.Fatalf("A: %v %s", err, out)
	}
	out, err := swarm(t, prof, 30*time.Second, "task", "add", "--repo", "app", "--run", "1", "--title", "B",
		"--spec", "b", "--acceptance", "go test passes", "--needs", "1")
	if err != nil || !strings.Contains(out, "task 2 draft") {
		t.Fatalf("B: %v %s", err, out)
	}
	if out, err := swarm(t, prof, 2*time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if st := status(t, prof); !strings.Contains(st, "merged       2") {
		t.Fatalf("want both merged:\n%s", st)
	}
	// B's commit sits on top of A's: A merged first, B was based on or rebased onto it.
	if log := sh(t, repo, "git log --format=%s main"); !strings.HasPrefix(log, "T2\nT1\n") {
		t.Fatalf("order on main:\n%s", log)
	}
	if _, err := swarm(t, prof, 30*time.Second, "task", "add", "--repo", "app", "--title", "C",
		"--spec", "c", "--acceptance", "x", "--needs", "99"); err == nil {
		t.Fatal("a dependency on a task that does not exist was accepted")
	}
}

// swarm gate in a worker refuses what the daemon's gate would refuse: the worker learns
// before submitting, not after a wasted attempt.
func TestWorkerGateCatchesWhatTheDaemonWould(t *testing.T) {
	repo := testbed(t)
	marks := t.TempDir()
	worker := fmt.Sprintf(`printf 'package app\n\nimport "testing"\n\nfunc TestOne(t *testing.T) { t.Skip() }\n' > one_test.go
git add -A && git commit -qm skip
if swarm gate > %q 2>&1; then echo pass >> %q; fi
exit 0
`, filepath.Join(marks, "gate.out"), filepath.Join(marks, "gate.out"))
	prof := profile(t, repo, worker, 1)
	addTask(t, prof, "")
	if out, err := swarm(t, prof, time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(filepath.Join(marks, "gate.out"))
	if !strings.Contains(string(got), "a test was switched off") || strings.Contains(string(got), "pass\n") {
		t.Fatalf("swarm gate output:\n%s", got)
	}
}

// Two independent tasks of one run, each claiming its own file. The second is rebased after
// the first merges; its re-gate must not see the first one's file as its own change.
func TestClaimsHoldAfterRebase(t *testing.T) {
	repo := testbed(t)
	worker := `f="t$SWARM_TASK_ID.go"
printf 'package app\n\nfunc T%s() int { return %s }\n' "$SWARM_TASK_ID" "$SWARM_TASK_ID" > "$f"
git add -A && git commit -qm "T$SWARM_TASK_ID"
swarm board done "$(git rev-parse HEAD)"
`
	prof := profile(t, repo, worker, 2)
	for _, a := range [][]string{
		{"task", "add", "--repo", "app", "--title", "A", "--spec", "a", "--acceptance", "x", "--files", "t1.go"},
		{"task", "add", "--repo", "app", "--run", "1", "--title", "B", "--spec", "b", "--acceptance", "x", "--files", "t2.go"},
	} {
		if out, err := swarm(t, prof, 30*time.Second, a...); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	out, _ := swarm(t, prof, 2*time.Minute, "daemon", "--until-idle")
	if st := status(t, prof); !strings.Contains(st, "merged       2") {
		t.Fatalf("want both merged:\n%s\n%s", st, out)
	}
}

// Two tasks claiming the same directory never hold workers at the same time, even with
// two slots free: each worker checks for the other's lock while it works.
func TestOverlappingClaimsNeverRunTogether(t *testing.T) {
	repo := testbed(t)
	marks := t.TempDir()
	clash := filepath.Join(marks, "clash")
	worker := fmt.Sprintf(`touch %[1]s/lock.$SWARM_TASK_ID
sleep 1
if ls %[1]s/lock.* | grep -v "lock.$SWARM_TASK_ID$" >/dev/null; then touch %[2]q; fi
mkdir -p pkg && printf 'package pkg\n\nfunc T%%s() {}\n' "$SWARM_TASK_ID" > "pkg/t$SWARM_TASK_ID.go"
rm -f %[1]s/lock.$SWARM_TASK_ID
git add -A && git commit -qm "T$SWARM_TASK_ID"
swarm board done "$(git rev-parse HEAD)"
`, marks, clash)
	prof := profile(t, repo, worker, 2)
	for _, a := range [][]string{
		{"task", "add", "--repo", "app", "--title", "A", "--spec", "a", "--acceptance", "x", "--files", "pkg/"},
		{"task", "add", "--repo", "app", "--run", "1", "--title", "B", "--spec", "b", "--acceptance", "x", "--files", "pkg/"},
	} {
		if out, err := swarm(t, prof, 30*time.Second, a...); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	if out, err := swarm(t, prof, 2*time.Minute, "daemon", "--until-idle"); err != nil {
		t.Fatalf("daemon: %v\n%s", err, out)
	}
	if _, err := os.Stat(clash); err == nil {
		t.Fatal("two workers held overlapping claims at the same time")
	}
	if st := status(t, prof); !strings.Contains(st, "merged       2") {
		t.Fatalf("want both merged:\n%s", st)
	}
}
