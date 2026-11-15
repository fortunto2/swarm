package board

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/taskbase"
)

func TestWorkerTokenCannotUseSessionTools(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := &Service{DB: d, BaseFor: func(string) (string, error) { return "abc", nil }}
	worker := s.Issue(Identity{TaskID: 1, Role: "worker"})
	ctx := context.Background()
	if _, err := s.AddTask(ctx, worker, AddTaskIn{Repo: "r", Title: "t", Spec: "s", Acceptance: "a", Files: []string{"**"}}); AsError(err).Code != "forbidden" {
		t.Fatalf("worker added a task: %v", err)
	}
	if _, err := s.Status(ctx, "not-a-token"); AsError(err).Code != "bad_token" {
		t.Fatalf("unknown token: %v", err)
	}
	op, err := OperatorToken(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.IssueFixed(op, Identity{Role: "lead"})
	if _, err := s.AddTask(ctx, op, AddTaskIn{Repo: "r", Title: "t", Spec: "s", Acceptance: "a", Files: []string{"**"}}); err != nil {
		t.Fatalf("operator could not add: %v", err)
	}
	if _, err := s.AddTask(ctx, op, AddTaskIn{Repo: "r", Title: "t", Spec: "s"}); AsError(err).Code != "incomplete" {
		t.Fatalf("task without acceptance accepted: %v", err)
	}
	s.Strict = true
	if _, err := s.AddTask(ctx, op, AddTaskIn{Repo: "r", Title: "t", Spec: "s", Acceptance: "a"}); AsError(err).Code != "no_files" {
		t.Fatalf("strict: task without files accepted: %v", err)
	}
	s.Strict = false
	if _, err := s.AddTask(ctx, op, AddTaskIn{Repo: "r", Title: "t", Spec: "s", Acceptance: "a"}); err != nil {
		t.Fatalf("trusted: a task without files takes the whole repo: %v", err)
	}
}

// The op table enforces roles for every operation, whatever the service method checks.
func TestOpTableEnforcesRoles(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := &Service{DB: d}
	worker := s.Issue(Identity{TaskID: 1, Role: "worker"})
	lead := s.Issue(Identity{Role: "lead"})
	ctx := context.Background()
	for _, o := range ops {
		other := worker
		if o.role == "worker" {
			other = lead
		}
		if _, err := s.Do(ctx, o.name, other, nil); AsError(err).Code != "forbidden" {
			t.Errorf("%s with the wrong role: %v", o.name, err)
		}
		if o.schema == nil {
			t.Errorf("%s has no input schema", o.name)
		}
	}
	if _, err := s.Do(ctx, "nope", lead, nil); AsError(err).Code != "unknown_op" {
		t.Fatalf("unknown op: %v", err)
	}
}

// The socket closes a connection that sends more than its budget, and the daemon lives on.
func TestSocketCapsAConnection(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := &Service{DB: d}
	sock := filepath.Join(t.TempDir(), "b.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, sock, s) }()
	var c net.Conn
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c == nil {
		t.Fatal(err)
	}
	junk := make([]byte, 1<<20)
	for i := range junk {
		junk[i] = ' '
	}
	var werr error
	for i := 0; i < 40 && werr == nil; i++ { // 40 MB of whitespace: never a complete request
		_, werr = c.Write(junk)
	}
	if werr == nil {
		t.Fatal("a 40 MB stream was accepted on one connection")
	}
	cl, err := Dial(sock) // the door still opens for the next caller
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if _, err := cl.Do("get_task", "nope", nil); AsError(err).Code != "bad_token" {
		t.Fatalf("daemon not answering after the flood: %v", err)
	}
}

// A lead widens a blocked task's files (a test asserting the old behaviour, outside the
// claims); a finished task is not widened.
func TestClaimFilesWidensATaskNotAFinishedOne(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	s := &Service{DB: d}
	lead := s.Issue(Identity{Role: "lead"})
	run, _ := d.CreateRun(ctx, "x", "", "base")
	id, _ := d.AddTask(ctx, db.NewTask{RunID: run, Repo: "app", Title: "T", Spec: "s", Acceptance: "a", Tier: "B", Files: []string{"a.go"}})
	out, err := s.ClaimFiles(ctx, lead, id, []string{"a_test.go", " ", "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := d.WriteClaims(ctx, id); len(got) != 2 || got[0] != "a.go" || got[1] != "a_test.go" {
		t.Fatalf("claims after widening: %v (%v)", got, out)
	}
	if _, err := s.ClaimFiles(ctx, lead, id, nil); AsError(err).Code != "bad_input" {
		t.Fatalf("no files: %v", err)
	}
	if err := d.Move(ctx, "t", id, []db.Status{db.Ready}, db.Cancelled, db.Patch{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimFiles(ctx, lead, id, []string{"b.go"}); AsError(err).Code != "finished" {
		t.Fatalf("a cancelled task was widened: %v", err)
	}
	worker := s.Issue(Identity{TaskID: id, Role: "worker"})
	if _, err := s.Do(ctx, "swarm_claim_files", worker, []byte(`{"task_id":1,"files":["x"]}`)); AsError(err).Code != "forbidden" {
		t.Fatalf("a worker widened its own claims: %v", err)
	}
}

// A worker blocked on a question was answered by doing what it asked in its worktree; the
// retry could not say so, and the next attempt had to guess from the git state (found live).
// The note reaches the next attempt as a review note, which its prompt carries.
func TestRetryNoteReachesTheNextAttempt(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	s := &Service{DB: d}
	lead := s.Issue(Identity{Role: "lead"})
	run, _ := d.CreateRun(ctx, "x", "", "base")
	id, _ := d.AddTask(ctx, db.NewTask{RunID: run, Repo: "app", Title: "T", Spec: "s", Acceptance: "a", Tier: "B", Files: []string{"a.go"}})
	block := func() {
		t.Helper()
		if err := d.Move(ctx, "t", id, []db.Status{db.Ready}, db.Running, db.Patch{}); err != nil {
			t.Fatal(err)
		}
		if err := d.Move(ctx, "t", id, []db.Status{db.Running}, db.Blocked, db.Patch{}); err != nil {
			t.Fatal(err)
		}
	}
	block()
	if _, err := s.Do(ctx, "swarm_retry_task", lead, []byte(fmt.Sprintf(`{"task_id":%d,"note":"  done: your tree is on the new base  "}`, id))); err != nil {
		t.Fatal(err)
	}
	notes, _ := d.Notes(ctx, id, "review", 4)
	if len(notes) != 1 || !strings.Contains(notes[0].Body, "done: your tree is on the new base") {
		t.Fatalf("review notes after a retry with a note: %+v", notes)
	}
	block()
	if _, err := s.RetryTask(ctx, lead, id, " "); err != nil {
		t.Fatal(err)
	}
	if notes, _ := d.Notes(ctx, id, "review", 4); len(notes) != 1 {
		t.Fatalf("a blank note was stored: %+v", notes)
	}
}

// A cancelled task's joined base (refs/swarm/base/T<id>-<sha>) is never fetched again: the
// ref goes with it, as it does on merge.
func TestCancelForgetsTheTasksBaseRefs(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	repo := t.TempDir()
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "--allow-empty", "-m", "base"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	s := &Service{DB: d, Repo: func(string) (config.Repo, error) { return config.Repo{Path: repo, MainBranch: "main"}, nil }}
	lead := s.Issue(Identity{Role: "lead"})
	run, _ := d.CreateRun(ctx, "x", "", "base")
	id, _ := d.AddTask(ctx, db.NewTask{RunID: run, Repo: "app", Title: "T", Spec: "s", Acceptance: "a", Tier: "B", Files: []string{"a.go"}})
	ref := taskbase.Ref(id, "0123456789abcdef")
	if out, err := exec.Command("git", "-C", repo, "update-ref", ref, "main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := s.CancelTask(ctx, lead, id); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("git", "-C", repo, "for-each-ref", "refs/swarm/base/").Output(); len(out) != 0 {
		t.Fatalf("base ref left after cancel: %s", out)
	}
}
