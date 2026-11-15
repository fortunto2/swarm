package taskbase

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", file)
	return git(t, dir, "rev-parse", "HEAD")
}

// bed: a repo whose main has a.txt, a dependency T1 delivered as a branch (merged in swarm,
// not in main), and the dependent T2.
type bed struct {
	dir  string
	d    *db.DB
	repo config.Repo
	dep  db.Task
	task db.Task
}

func newBed(t *testing.T, merge string) *bed {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	commit(t, dir, "a.txt", "a\n")
	d, err := db.Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()
	if _, err := d.ExecContext(ctx, `INSERT INTO runs(spec_path,status,base_commit,created_at) VALUES('s','running','x',unixepoch())`); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"merged", "ready"} {
		if _, err := d.ExecContext(ctx, `INSERT INTO tasks(run_id,repo,title,spec,acceptance,tier,status,created_at,updated_at)
			VALUES(1,'r','t','s','a','B',?,unixepoch(),unixepoch())`, st); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO deps(task_id, needs_id) VALUES(2, 1)`); err != nil {
		t.Fatal(err)
	}
	return &bed{dir: dir, d: d, repo: config.Repo{Path: dir, MainBranch: "main", Merge: merge},
		dep: db.Task{ID: 1, Repo: "r"}, task: db.Task{ID: 2, Repo: "r"}}
}

// deliver: T1's work on a branch from main, recorded as its result commit.
func (b *bed) deliver(t *testing.T, file, body string) string {
	t.Helper()
	git(t, b.dir, "checkout", "-q", "-b", "swarm/done/T1")
	c := commit(t, b.dir, file, body)
	git(t, b.dir, "checkout", "-q", "main")
	if _, err := b.d.ExecContext(context.Background(), `UPDATE tasks SET result_commit=? WHERE id=1`, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBaseJoinsMainWithTheDependencysDelivery(t *testing.T) {
	ctx := context.Background()
	b := newBed(t, "branch")
	dep := b.deliver(t, "b.txt", "b\n")
	main := commit(t, b.dir, "c.txt", "c\n") // main moved after the delivery
	base, ref, err := For(ctx, b.d, b.repo, b.task)
	if err != nil {
		t.Fatal(err)
	}
	if ref != Ref(2, base) || git(t, b.dir, "rev-parse", ref) != base {
		t.Fatalf("ref %q, want %s pointing at %s", ref, Ref(2, base), base)
	}
	if !gitx.IsAncestor(ctx, b.dir, main, base) || !gitx.IsAncestor(ctx, b.dir, dep, base) {
		t.Fatalf("base %s does not hold both main %s and the delivery %s", base, main, dep)
	}
	if files := git(t, b.dir, "ls-tree", "--name-only", base); files != "a.txt\nb.txt\nc.txt" {
		t.Fatalf("base tree = %q", files)
	}
	time.Sleep(1100 * time.Millisecond) // a join stamped with the clock would differ now
	again, _, err := For(ctx, b.d, b.repo, b.task)
	if err != nil || again != base {
		t.Fatalf("second call gave %s (%v), want the same %s: start, gate and merge must agree", again, err, base)
	}
}

func TestBaseIsTheDeliveryWhenMainHasNotMoved(t *testing.T) {
	b := newBed(t, "branch")
	dep := b.deliver(t, "b.txt", "b\n")
	base, ref, err := For(context.Background(), b.d, b.repo, b.task)
	if err != nil || base != dep || ref != Ref(2, dep) {
		t.Fatalf("base %s ref %q (%v), want the delivery %s", base, ref, err, dep)
	}
}

func TestBaseIsMainOnceTheDeliveryLanded(t *testing.T) {
	b := newBed(t, "branch")
	b.deliver(t, "b.txt", "b\n")
	git(t, b.dir, "merge", "-q", "--ff-only", "swarm/done/T1")
	main := commit(t, b.dir, "c.txt", "c\n")
	base, ref, err := For(context.Background(), b.d, b.repo, b.task)
	if err != nil || base != main || ref != "" {
		t.Fatalf("base %s ref %q (%v), want main %s and no ref", base, ref, err, main)
	}
}

// A delivery that no longer joins with main is an error naming it, never a base without main
// (an earlier version took the delivery alone; a blind review showed it then never rejoined).
func TestBaseConflictIsAnErrorNamingTheDependency(t *testing.T) {
	b := newBed(t, "branch")
	b.deliver(t, "a.txt", "dependency\n")
	commit(t, b.dir, "a.txt", "main\n")
	_, _, err := For(context.Background(), b.d, b.repo, b.task)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "T1's delivery") {
		t.Fatalf("err = %v, want ErrConflict naming T1", err)
	}
}

// Two dependencies: the base holds both and main.
func TestBaseJoinsEveryDependency(t *testing.T) {
	ctx := context.Background()
	b := newBed(t, "branch")
	b.deliver(t, "b.txt", "b\n")
	if _, err := b.d.ExecContext(ctx, `INSERT INTO tasks(run_id,repo,title,spec,acceptance,tier,status,created_at,updated_at)
		VALUES(1,'r','t','s','a','B','merged',unixepoch(),unixepoch())`); err != nil {
		t.Fatal(err)
	}
	git(t, b.dir, "checkout", "-q", "-b", "swarm/done/T3", "main")
	second := commit(t, b.dir, "d.txt", "d\n")
	git(t, b.dir, "checkout", "-q", "main")
	if _, err := b.d.ExecContext(ctx, `UPDATE tasks SET result_commit=? WHERE id=3`, second); err != nil {
		t.Fatal(err)
	}
	if _, err := b.d.ExecContext(ctx, `INSERT INTO deps(task_id, needs_id) VALUES(2, 3)`); err != nil {
		t.Fatal(err)
	}
	commit(t, b.dir, "c.txt", "c\n")
	base, _, err := For(ctx, b.d, b.repo, b.task)
	if err != nil {
		t.Fatal(err)
	}
	if files := git(t, b.dir, "ls-tree", "--name-only", base); files != "a.txt\nb.txt\nc.txt\nd.txt" {
		t.Fatalf("base tree = %q, want main and both deliveries", files)
	}
}

// git failing for another reason (here: unrelated histories, exit 128) is an error, not a
// conflict: reading any non-zero exit as a conflict once dropped main from the base.
func TestJoinFailureIsNotAConflict(t *testing.T) {
	ctx := context.Background()
	b := newBed(t, "branch")
	git(t, b.dir, "checkout", "-q", "--orphan", "other")
	other := commit(t, b.dir, "z.txt", "z\n")
	git(t, b.dir, "checkout", "-q", "-f", "main")
	main := git(t, b.dir, "rev-parse", "main")
	_, err := gitx.Join(ctx, b.dir, main, other, "x")
	if err == nil || errors.Is(err, gitx.ErrConflict) {
		t.Fatalf("join of unrelated histories = %v, want a plain error", err)
	}
}

func TestForgetDeletesOnlyThisTasksRefs(t *testing.T) {
	ctx := context.Background()
	b := newBed(t, "branch")
	head := git(t, b.dir, "rev-parse", "main")
	for _, ref := range []string{Ref(1, head), Ref(10, head), Ref(1, "abcdef")} {
		git(t, b.dir, "update-ref", ref, head)
	}
	Forget(ctx, b.dir, 1)
	if got := git(t, b.dir, "for-each-ref", "--format=%(refname)", "refs/swarm/base/"); got != Ref(10, head) {
		t.Fatalf("left %q, want only T10's", got)
	}
}

func TestBaseIsMainWithFastForwardMerges(t *testing.T) {
	b := newBed(t, "ff")
	b.deliver(t, "b.txt", "b\n")
	main := git(t, b.dir, "rev-parse", "main")
	base, ref, err := For(context.Background(), b.d, b.repo, b.task)
	if err != nil || base != main || ref != "" {
		t.Fatalf("ff: base %s ref %q (%v), want main %s", base, ref, err, main)
	}
}

func TestBaseIgnoresAnUnmergedDependency(t *testing.T) {
	b := newBed(t, "branch")
	b.deliver(t, "b.txt", "b\n")
	if _, err := b.d.ExecContext(context.Background(), `UPDATE tasks SET status='ready' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	main := git(t, b.dir, "rev-parse", "main")
	base, _, err := For(context.Background(), b.d, b.repo, b.task)
	if err != nil || base != main {
		t.Fatalf("base %s (%v), want main %s: only merged dependencies count", base, err, main)
	}
}

// Main moves while a task on a joined base works: the new base and the task's commit then
// have two merge bases, and a diff from the new base takes in the dependency's file, which
// the gate would fail as outside the task's claims. From the base the task started on, the
// diff is its own (reproduced by hand first: git picked the merge base that showed b.txt).
func TestGateMeasuresATaskOnAJoinedBaseFromItsOwnBase(t *testing.T) {
	ctx := context.Background()
	b := newBed(t, "branch")
	b.deliver(t, "b.txt", "b\n")
	commit(t, b.dir, "c.txt", "c\n")
	start, _, err := For(ctx, b.d, b.repo, b.task)
	if err != nil {
		t.Fatal(err)
	}
	git(t, b.dir, "checkout", "-q", "-b", "swarm/T2", start)
	work := commit(t, b.dir, "e.txt", "e\n")
	git(t, b.dir, "checkout", "-q", "main")
	commit(t, b.dir, "f.txt", "f\n") // main moves
	now, _, err := For(ctx, b.d, b.repo, b.task)
	if err != nil || now == start {
		t.Fatalf("base did not follow main: %s (%v)", now, err)
	}
	// a node sent the first base fetches it by its ref after main moved: still that commit
	if got := git(t, b.dir, "rev-parse", Ref(2, start)); got != start {
		t.Fatalf("the first base's ref now points at %s, want %s", got, start)
	}
	claims := []string{"e.txt"}
	if v := gate.Static(ctx, b.dir, now, work, claims, gate.Grants{}); v.OK {
		t.Fatal("git chose the merge base that hides the dependency: this test no longer pins StaticFrom; rebuild the case")
	}
	if v := gate.StaticFrom(ctx, b.dir, []string{now, start}, work, claims, gate.Grants{}); !v.OK {
		t.Fatalf("measured from the task's own base: %s %s", v.Reason, v.Tail)
	}
	if v := gate.StaticFrom(ctx, b.dir, []string{now, start}, work, []string{"x.txt"}, gate.Grants{}); v.OK {
		t.Fatal("a commit outside its claims passed: the second base must not widen what is allowed")
	}
}
