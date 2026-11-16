package main

// Nodes, end to end through the real binary: a coordinator daemon and a `swarm node` on the
// same machine, linked by the same command a remote node uses minus ssh. The worker runs
// in the node's own clone; its branch reaches the coordinator on report_done; the gate
// runs on the node; the coordinator fast-forwards its main.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type safeBuf struct {
	mu  chanMutex
	buf bytes.Buffer
}

type chanMutex chan struct{}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu <- struct{}{}
	defer func() { <-b.mu }()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu <- struct{}{}
	defer func() { <-b.mu }()
	return b.buf.String()
}

// nodeBed: a coordinator profile whose only account is on remote node n1, and n1's own
// profile with a clone of the repo. worker is the mock worker's script on the node.
func nodeBed(t *testing.T, worker, gateOn, extraCoord string) (coord, nodeProf, repo, clone string) {
	t.Helper()
	repo = testbed(t)
	clone = filepath.Join(t.TempDir(), "clone")
	sh(t, filepath.Dir(clone), fmt.Sprintf("git clone -q %s %s", repo, clone))
	coord = t.TempDir()
	script := filepath.Join(coord, "worker.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nset -e\n"+worker), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf(`
[profile]
name = "coord"
worktree_root = %q

[repos.app]
path = %q
gate = ["go vet ./...", "go test ./... -count=1"]
gate_timeout = "2m"
merge = "ff"
gate_on = %q

[limits]
max_workers = 2
task_wall_clock = "3m"   # an attempt runs go vet and go test twice; a loaded machine took over 1m
max_attempts = 3
node_timeout = "3s"

[providers.mock]
kind = "mock"
family = "mock"
slots = 1
command = [%q]

[nodes.n1]
host = "remote"
accounts = ["mock"]
labels = ["far"]
%s`, filepath.Join(coord, "wt"), repo, gateOn, script, extraCoord)
	if err := os.WriteFile(filepath.Join(coord, "swarm.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	nodeProf = t.TempDir()
	ntoml := fmt.Sprintf(`
[profile]
name = "n1"
worktree_root = %q

[node]
name = "n1"
coordinator = [%q, "--profile", %q]

[repos.app]
path = %q
remote = %q
`, filepath.Join(nodeProf, "wt"), bin, coord, clone, repo)
	if err := os.WriteFile(filepath.Join(nodeProf, "swarm.toml"), []byte(ntoml), 0o600); err != nil {
		t.Fatal(err)
	}
	return coord, nodeProf, repo, clone
}

func startNode(t *testing.T, nodeProf string) (*exec.Cmd, *safeBuf) {
	t.Helper()
	log := &safeBuf{mu: make(chanMutex, 1)}
	n := exec.Command(bin, "--profile", nodeProf, "node")
	n.Stdout, n.Stderr = log, log
	// its own group, killed whole: a git fetch it started outlived a kill of the node alone and
	// was still writing into the clone's .git when the test removed it (seen under full load)
	n.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-n.Process.Pid, syscall.SIGKILL); _ = n.Wait() })
	return n, log
}

func sqlite(t *testing.T, prof, q string) string {
	t.Helper()
	out, err := exec.Command("sqlite3", filepath.Join(prof, "state", "swarm.db"), q).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 %s: %v\n%s", q, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestNodeRunsTheWorkerAndTheGate(t *testing.T) {
	// a local account first in line: without runs_on the task would run here
	coord, nodeProf, repo, clone := nodeBed(t, goodWorker, "node", `
[providers.here]
kind = "mock"
family = "mock"
slots = 1
priority = -1
command = ["/bin/false"]
`)
	before := sh(t, repo, "git rev-parse main")
	if out, err := swarm(t, coord, 30*time.Second, "task", "add", "--repo", "app", "--title", "add Two",
		"--spec", "add func Two() int returning 2, with a test", "--acceptance", "go test passes",
		"--files", "two.go,two_test.go", "--runs-on", "far"); err != nil {
		t.Fatalf("task add: %v\n%s", err, out)
	}
	if out, err := swarm(t, coord, 30*time.Second, "task", "add", "--repo", "app", "--title", "x", "--spec", "x",
		"--acceptance", "x", "--files", "x.go", "--runs-on", "gpu"); err == nil || !strings.Contains(out, "no node has that label") {
		t.Fatalf("a label no node has was accepted: %v\n%s", err, out)
	}
	seen := filepath.Join(t.TempDir(), "statuses")
	sh(t, nodeProf, fmt.Sprintf(`printf '\n[hooks]\non_status = ["echo {task} {status} >> %s"]\n' >> swarm.toml`, seen))
	if out, err := swarm(t, nodeProf, 30*time.Second, "node", "--check"); err == nil {
		t.Fatalf("a check with no coordinator running passed: %s", out)
	}
	_, nlog := startNode(t, nodeProf)
	daemon := exec.Command(bin, "--profile", coord, "daemon", "--until-idle")
	var dlog bytes.Buffer
	daemon.Stdout, daemon.Stderr = &dlog, &dlog
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- daemon.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon: %v\n%s\nnode:\n%s", err, dlog.String(), nlog.String())
		}
	case <-time.After(3 * time.Minute):
		_ = daemon.Process.Kill()
		t.Fatalf("not done in 3 min\n%s\nnode:\n%s\n%s", dlog.String(), nlog.String(), status(t, coord))
	}
	if st := status(t, coord); !strings.Contains(st, "merged       1") {
		logs, _ := exec.Command("sh", "-c", "tail -n 30 "+filepath.Join(coord, "state", "attempts", "*", "log.jsonl")).CombinedOutput()
		t.Fatalf("not merged:\n%s\ndaemon:\n%s\nnode:\n%s\nattempt logs:\n%s", st, dlog.String(), nlog.String(), logs)
	}
	if sh(t, repo, "git rev-parse main") == before {
		t.Fatal("the coordinator's main did not move")
	}
	if msg := sh(t, repo, "git log -1 --format=%an main"); msg != "swarm-mock-T1-1" {
		t.Fatalf("merged commit is not the node worker's: %q", msg)
	}
	if n := sqlite(t, coord, "SELECT group_concat(node) FROM attempts WHERE task_id=1"); n != "n1" {
		t.Fatalf("attempts ran on %q: runs_on = far must keep the task off the local node", n)
	}
	if g := sqlite(t, coord, "SELECT data FROM events WHERE type='gate_started'"); !strings.Contains(g, `"node":"n1"`) {
		t.Fatalf("the gate did not run on the node: %s", g)
	}
	if !strings.Contains(nlog.String(), `"msg":"gate started"`) {
		t.Fatalf("the node log shows no gate:\n%s", nlog.String())
	}
	// the worktree was the node's, never the coordinator's
	if _, err := os.Stat(filepath.Join(coord, "wt", "app--T1")); err == nil {
		t.Fatal("a worktree was made on the coordinator")
	}
	sh(t, clone, "git rev-parse --verify -q refs/swarm/main/app >/dev/null") // the node fetched the coordinator's main
	// the node's own status hooks heard its task move (a board or card on that machine)
	waitFor(t, 10*time.Second, func() bool {
		b, _ := os.ReadFile(seen)
		return strings.Contains(string(b), "1 running") && strings.Contains(string(b), "1 verifying")
	}, "the node's on_status hooks did not hear the task move")
	if st := status(t, coord); !strings.Contains(st, "node n1") || !strings.Contains(st, "last seen") {
		t.Fatalf("status does not show when the node was last heard from:\n%s", st)
	}
	// a node on another build than the coordinator says so (the same binary here: silent)
	if st := status(t, coord); strings.Contains(st, "runs swarm") {
		t.Fatalf("a node on the coordinator's own build is flagged:\n%s", st)
	}
	if v, _ := swarm(t, coord, 10*time.Second, "version"); !strings.Contains(v, "unknown") {
		sqlite(t, coord, `UPDATE nodes SET info=json_set(info, '$.version', '0000000') WHERE id='n1'`)
		if st := status(t, coord); !strings.Contains(st, "runs swarm 0000000, this binary is") {
			t.Fatalf("a node on an older build is not flagged:\n%s", st)
		}
	}
	if out, err := swarm(t, nodeProf, 30*time.Second, "node", "--check"); err == nil {
		t.Fatalf("a check passed with the coordinator's daemon gone: %s", out)
	}
	if st := status(t, nodeProf); !strings.Contains(st, "node n1 (profile n1): running") {
		t.Fatalf("a node profile's status:\n%s", st)
	}
	if out, _ := swarm(t, coord, 30*time.Second, "task", "list"); !strings.Contains(out, "n1") || !strings.Contains(out, "[runs_on far]") {
		t.Fatalf("task list does not say where the task ran and what it asked for:\n%s", out)
	}
	if out, _ := swarm(t, coord, 30*time.Second, "providers"); !strings.Contains(out, "mock           n1") {
		t.Fatalf("providers does not say mock runs on n1:\n%s", out)
	}
}

// A node that goes away mid-attempt: past twice node_timeout the attempt is lost and the
// task goes back to ready, not stuck running; when the node is back it is run to the end.
func TestNodeLostMidAttemptGoesBackToReady(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "first")
	nap := fmt.Sprintf("sleep %d", 3000+time.Now().UnixNano()%6000) // this run's own, to find it by
	worker := fmt.Sprintf("if [ ! -f %q ]; then touch %q; %s; fi\n", mark, mark, nap) + goodWorker
	coord, nodeProf, repo, _ := nodeBed(t, worker, "coordinator", "")
	before := sh(t, repo, "git rev-parse main")
	addTask(t, coord, "two.go,two_test.go")
	n, nlog := startNode(t, nodeProf)
	daemon := exec.Command(bin, "--profile", coord, "daemon")
	var dlog bytes.Buffer
	daemon.Stdout, daemon.Stderr = &dlog, &dlog
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Process.Signal(os.Interrupt); _ = daemon.Wait() })
	waitFor(t, 30*time.Second, func() bool { _, err := os.Stat(mark); return err == nil }, "the first attempt never started")
	_ = n.Process.Kill() // the machine goes away without a word; its worker is left behind
	_ = n.Wait()
	find := "pgrep -fx '" + nap + "' || true"
	orphan := sh(t, ".", find)
	waitFor(t, 40*time.Second, func() bool { return strings.Contains(status(t, coord), "ready        1") },
		"the task did not come back to ready after its node went away")
	if out := sqlite(t, coord, "SELECT outcome FROM attempts WHERE task_id=1 ORDER BY id LIMIT 1"); out != "crash" {
		t.Fatalf("the lost attempt ended as %q, want crash (the infrastructure's, not the model's)", out)
	}
	if orphan == "" {
		t.Fatal("the hard-killed node left no worker behind: this test no longer tests the orphan")
	}
	_, nlog2 := startNode(t, nodeProf)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("status:\n%s\ndaemon:\n%s\nnode before:\n%s\nnode after:\n%s", status(t, coord), dlog.String(), nlog.String(), nlog2.String())
		}
	})
	waitFor(t, 15*time.Second, func() bool { return sh(t, ".", find) == "" },
		"the restarted node did not stop the orphaned worker")
	// generous: under a loaded machine (the whole suite in parallel) the second attempt and
	// its gate have taken over two minutes
	waitFor(t, 4*time.Minute, func() bool { return strings.Contains(status(t, coord), "merged       1") },
		"not merged after the node came back")
	if sh(t, repo, "git rev-parse main") == before {
		t.Fatal("main did not move")
	}
	_ = nlog
}

func waitFor(t *testing.T, limit time.Duration, ok func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// gate_on = "node": a node lost mid-gate leaves the task verifying (no gate_fail, no gate
// here, where the suite may not run), and the gate runs again on the node when it is back.
func TestNodeGateWaitsForItsNode(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "gated")
	coord, nodeProf, repo, _ := nodeBed(t, goodWorker, "node", "")
	// the node's gate leaves a mark, then takes a while: time to pull the plug mid-gate
	sh(t, coord, fmt.Sprintf(`sed -i.bak 's|^gate = .*|gate = ["touch %s", "sleep 4", "go test ./... -count=1"]|' swarm.toml`, mark))
	before := sh(t, repo, "git rev-parse main")
	addTask(t, coord, "two.go,two_test.go")
	n, _ := startNode(t, nodeProf)
	daemon := exec.Command(bin, "--profile", coord, "daemon")
	var dlog bytes.Buffer
	daemon.Stdout, daemon.Stderr = &dlog, &dlog
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Process.Signal(os.Interrupt); _ = daemon.Wait() })
	// the link check, beside the running node: answers, and takes nothing from it
	waitFor(t, 30*time.Second, func() bool {
		out, err := swarm(t, nodeProf, 30*time.Second, "node", "--check")
		return err == nil && strings.Contains(out, "ok: the coordinator answers") && strings.Contains(out, "knows node n1")
	}, "swarm node --check does not pass against a running coordinator")
	gated := func() bool {
		return sqlite(t, coord, "SELECT count(*) FROM events WHERE type='gate_started'") != "0"
	}
	// the worker's own `swarm gate` touches the mark too; wait for the daemon's gate
	waitFor(t, time.Minute, gated, "the gate never started")
	time.Sleep(time.Second)
	_ = n.Process.Kill()
	_ = n.Wait()
	waitFor(t, 40*time.Second, func() bool {
		return sqlite(t, coord, "SELECT count(*) FROM events WHERE type='gate_again'") != "0"
	}, "no gate_again after the node went away mid-gate")
	if st := status(t, coord); !strings.Contains(st, "verifying    1") {
		t.Fatalf("want the task still verifying while its node is away:\n%s\n%s", st, dlog.String())
	}
	if f := sqlite(t, coord, "SELECT count(*) FROM events WHERE type IN ('gate_fail','gate_pass')"); f != "0" {
		t.Fatalf("a verdict was recorded without the node: %s events", f)
	}
	startNode(t, nodeProf)
	waitFor(t, 2*time.Minute, func() bool { return strings.Contains(status(t, coord), "merged       1") },
		"not merged after the node came back")
	if sh(t, repo, "git rev-parse main") == before {
		t.Fatal("main did not move")
	}
	if g := sqlite(t, coord, "SELECT count(*) FROM events WHERE type='gate_started' AND data LIKE '%\"node\":\"n1\"%'"); g != "2" {
		t.Fatalf("want two gates, both on n1; got %s", g)
	}
}

// swarm drain: what runs finishes and merges, nothing new starts, and the daemon exits 0
// for its supervisor to start the new binary.
func TestDrainFinishesInFlightWorkThenExits(t *testing.T) {
	repo := testbed(t)
	prof := profile(t, repo, "sleep 3\n"+goodWorker, 3)
	addTask(t, prof, "two.go,two_test.go")
	daemon := exec.Command(bin, "--profile", prof, "daemon")
	var dlog bytes.Buffer
	daemon.Stdout, daemon.Stderr = &dlog, &dlog
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- daemon.Wait() }()
	t.Cleanup(func() { _ = daemon.Process.Kill() })
	waitFor(t, 30*time.Second, func() bool { return strings.Contains(status(t, prof), "running      1") }, "the task never started")
	if out, err := swarm(t, prof, 30*time.Second, "drain"); err != nil || !strings.Contains(out, `"draining":true`) {
		t.Fatalf("drain: %v %s", err, out)
	}
	if st := status(t, prof); !strings.Contains(st, "draining:") {
		t.Fatalf("status does not say it drains:\n%s", st)
	}
	if out, err := swarm(t, prof, 30*time.Second, "task", "add", "--repo", "app", "--title", "later", "--spec", "s",
		"--acceptance", "a", "--files", "later.go"); err != nil {
		t.Fatalf("task add while draining: %v %s", err, out)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the drained daemon exited with %v\n%s", err, dlog.String())
		}
	case <-time.After(3 * time.Minute):
		t.Fatalf("the daemon did not exit after draining\n%s\n%s", status(t, prof), dlog.String())
	}
	st := status(t, prof)
	if !strings.Contains(st, "merged       1") || !strings.Contains(st, "ready        1") {
		t.Fatalf("want the running task merged and the later one untouched:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(prof, "state", "draining")); err == nil {
		t.Fatal("the drain flag outlived the daemon that drained")
	}
}

// swarm drain on a node profile: the node takes no new work, waits for its worker, exits 0.
func TestNodeDrainWaitsForItsWorkerThenExits(t *testing.T) {
	coord, nodeProf, _, _ := nodeBed(t, "sleep 4\n"+goodWorker, "coordinator", "")
	addTask(t, coord, "two.go,two_test.go")
	n, nlog := startNode(t, nodeProf)
	ndone := make(chan error, 1)
	go func() { ndone <- n.Wait() }()
	daemon := exec.Command(bin, "--profile", coord, "daemon")
	var dlog bytes.Buffer
	daemon.Stdout, daemon.Stderr = &dlog, &dlog
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Process.Signal(os.Interrupt); _ = daemon.Wait() })
	waitFor(t, 30*time.Second, func() bool { return strings.Contains(nlog.String(), `"msg":"worker started"`) }, "no worker started on the node")
	if out, err := swarm(t, nodeProf, 30*time.Second, "drain"); err != nil || !strings.Contains(out, "draining") {
		t.Fatalf("node drain: %v %s", err, out)
	}
	select {
	case err := <-ndone:
		if err != nil {
			t.Fatalf("the drained node exited with %v\n%s", err, nlog.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("the node did not exit after draining\n%s", nlog.String())
	}
	if !strings.Contains(nlog.String(), `"msg":"worker exited"`) {
		t.Fatalf("the node exited before its worker did:\n%s", nlog.String())
	}
	waitFor(t, 2*time.Minute, func() bool { return strings.Contains(status(t, coord), "merged       1") },
		"the drained node's task did not merge")
}

// gate_on naming a node: a task this machine ran is gated there while the node is
// connected (a box's work on a laptop's cores), and here once it is gone.
func TestGateOnANamedNode(t *testing.T) {
	coord, nodeProf, repo, _ := nodeBed(t, "exit 1\n", "n1", "")
	// a local account, first in line, with the good worker: the task runs here
	sh(t, coord, fmt.Sprintf(`printf '\n[providers.here]\nkind = "mock"\nfamily = "mock"\nslots = 1\npriority = -1\ncommand = ["%s"]\n' >> swarm.toml`,
		filepath.Join(coord, "here.sh")))
	if err := os.WriteFile(filepath.Join(coord, "here.sh"), []byte("#!/bin/sh\nset -e\n"+goodWorker), 0o755); err != nil {
		t.Fatal(err)
	}
	before := sh(t, repo, "git rev-parse main")
	if out, err := swarm(t, coord, 30*time.Second, "init"); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	daemon := exec.Command(bin, "--profile", coord, "daemon")
	var dlog bytes.Buffer
	daemon.Stdout, daemon.Stderr = &dlog, &dlog
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Process.Signal(os.Interrupt); _ = daemon.Wait() })
	_, nlog := startNode(t, nodeProf)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("status:\n%s\ndaemon:\n%s\nnode:\n%s", status(t, coord), dlog.String(), nlog.String())
		}
	})
	waitFor(t, 30*time.Second, func() bool {
		for _, ln := range strings.Split(status(t, coord), "\n") {
			if strings.Contains(ln, "node n1 ") && strings.Contains(ln, " online") {
				return true
			}
		}
		return false
	}, "the node never connected")
	addTask(t, coord, "two.go,two_test.go")
	waitFor(t, 3*time.Minute, func() bool { return strings.Contains(status(t, coord), "merged       1") },
		"not merged")
	if sh(t, repo, "git rev-parse main") == before {
		t.Fatalf("main did not move\n%s\nnode:\n%s", dlog.String(), nlog.String())
	}
	if n := sqlite(t, coord, "SELECT group_concat(node) FROM attempts WHERE task_id=1"); n != "local" {
		t.Fatalf("the task ran on %q, want here", n)
	}
	if g := sqlite(t, coord, "SELECT data FROM events WHERE type='gate_started'"); !strings.Contains(g, `"node":"n1"`) {
		t.Fatalf("the gate of a task run here did not go to the named node: %s", g)
	}
}

// The gate on a named node ends with a review by that node's account: the first change is
// rejected there with a finding, the worker's next attempt reads it, the second merges.
func TestReviewOnTheGateNode(t *testing.T) {
	coord, nodeProf, repo, _ := nodeBed(t, "exit 1\n", "n1", "")
	marks := t.TempDir()
	reviewer := filepath.Join(marks, "reviewer.sh")
	if err := os.WriteFile(reviewer, []byte(fmt.Sprintf(`#!/bin/sh
cat > %[1]q/prompt.$$
if [ ! -f %[1]q/rejected ]; then touch %[1]q/rejected; printf 'two.go:1: wrong\nVERDICT: reject\n'; else printf 'VERDICT: approve\n'; fi
`, marks)), 0o755); err != nil {
		t.Fatal(err)
	}
	here := "printf 'package app\\n\\nfunc Two() int { return 2 }\\n' > two.go\n" +
		"printf 'package app\\n\\nimport \"testing\"\\n\\nfunc TestTwo(t *testing.T) { if Two() != 2 { t.Fatal() } }\\n' > two_test.go\n" +
		"git add -A && git commit -qm \"T$SWARM_TASK_ID: add Two\" --allow-empty\nswarm board done \"$(git rev-parse HEAD)\"\n"
	if err := os.WriteFile(filepath.Join(coord, "here.sh"), []byte("#!/bin/sh\nset -e\n"+here), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(coord, "swarm.toml"))
	s := strings.Replace(string(b), `gate_on = "n1"`, "gate_on = \"n1\"\nreview = \"rev\"", 1)
	s = strings.Replace(s, `accounts = ["mock"]`, `accounts = ["mock", "rev"]`, 1)
	s += fmt.Sprintf("\n[providers.here]\nkind = \"mock\"\nfamily = \"mock\"\nslots = 1\npriority = -1\ncommand = [%q]\n", filepath.Join(coord, "here.sh"))
	s += fmt.Sprintf("\n[providers.rev]\nkind = \"mock\"\nfamily = \"other\"\nroles = [\"reviewer\"]\nslots = 1\ncommand = [%q]\n", reviewer)
	if err := os.WriteFile(filepath.Join(coord, "swarm.toml"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := swarm(t, coord, 30*time.Second, "init"); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	daemon := exec.Command(bin, "--profile", coord, "daemon")
	var dlog bytes.Buffer
	daemon.Stdout, daemon.Stderr = &dlog, &dlog
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Process.Signal(os.Interrupt); _ = daemon.Wait() })
	_, nlog := startNode(t, nodeProf)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("status:\n%s\ndaemon:\n%s\nnode:\n%s", status(t, coord), dlog.String(), nlog.String())
		}
	})
	waitFor(t, 30*time.Second, func() bool { return strings.Contains(status(t, coord), " online") }, "the node never connected")
	before := sh(t, repo, "git rev-parse main")
	addTask(t, coord, "two.go,two_test.go")
	waitFor(t, 4*time.Minute, func() bool { return strings.Contains(status(t, coord), "merged       1") }, "not merged")
	if sh(t, repo, "git rev-parse main") == before {
		t.Fatal("main did not move")
	}
	if prompts, _ := filepath.Glob(filepath.Join(marks, "prompt.*")); len(prompts) != 2 {
		t.Fatalf("the node's reviewer ran %d times, want 2", len(prompts))
	}
	if n := sqlite(t, coord, "SELECT count(*) FROM events WHERE task_id=1 AND type='model_fail' AND data LIKE '%review_fail%'"); n != "1" {
		t.Fatalf("review_fail events = %s, want 1", n)
	}
}
