package db

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func seed(t *testing.T, d *DB, status string) int64 {
	t.Helper()
	ctx := context.Background()
	r, err := d.ExecContext(ctx, `INSERT INTO runs(spec_path,status,base_commit,created_at) VALUES('s.md','running','abc',unixepoch())`)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := r.LastInsertId()
	r, err = d.ExecContext(ctx, `INSERT INTO tasks(run_id,repo,title,spec,acceptance,tier,status,created_at,updated_at)
		VALUES(?,'r','t','s','a','B',?,unixepoch(),unixepoch())`, run, status)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := r.LastInsertId()
	return id
}

// PRD phase 0 acceptance: 50 goroutines, one task, exactly one winner.
func TestClaimOneWinner(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id := seed(t, d, "ready")

	var wins, taken atomic.Int32
	var other []error
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := d.Claim(context.Background(), id, "mock", "m", fmt.Sprintf("w%d", i), "local", time.Minute)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrTaken):
				taken.Add(1)
			default:
				mu.Lock()
				other = append(other, err)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(other) > 0 {
		t.Fatalf("unexpected errors (lock contention must not surface): %v", other[0])
	}
	if wins.Load() != 1 || taken.Load() != 49 {
		t.Fatalf("wins=%d taken=%d, want 1 and 49", wins.Load(), taken.Load())
	}
	var st, worker string
	if err := d.QueryRow(`SELECT status, worker_id FROM tasks WHERE id=?`, id).Scan(&st, &worker); err != nil {
		t.Fatal(err)
	}
	if st != "running" || worker == "" {
		t.Fatalf("status=%q worker=%q after claim", st, worker)
	}
}

// The instrument must be seen producing a non-zero: a task that is not ready is never claimed.
func TestClaimRefusesNotReady(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id := seed(t, d, "draft")
	if err := d.Claim(context.Background(), id, "mock", "m", "w", "local", time.Minute); !errors.Is(err, ErrTaken) {
		t.Fatalf("claim of a draft task: err=%v, want ErrTaken", err)
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "swarm.db")
	for range 2 {
		d, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		var mode string
		if err := d.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
			t.Fatalf("journal_mode=%q err=%v, want wal", mode, err)
		}
		d.Close()
	}
}

// The race the state machine exists for: a report_done (running -> verifying) lands between
// a reader seeing "running" and its failure write. The failure must lose, not overwrite.
func TestMoveLosesToAConcurrentTransition(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	id := seed(t, d, "running")
	if err := d.Move(ctx, "worker", id, []Status{Running}, Verifying, Patch{ResultCommit: P("abc")}); err != nil {
		t.Fatal(err)
	}
	err = d.Move(ctx, "swarmd", id, []Status{Running}, Ready, Patch{InfraFailures: P(1), ClearLease: true})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("stale failure write: err=%v, want ErrStale", err)
	}
	got, _ := d.GetTask(ctx, id)
	if got.Status != Verifying || got.ResultCommit.String != "abc" || got.InfraFailures != 0 {
		t.Fatalf("the winner was overwritten: %+v", got)
	}
}

func TestMoveRefusesIllegalTransition(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id := seed(t, d, "ready")
	if err := d.Move(context.Background(), "x", id, []Status{Ready}, Merged, Patch{}); !errors.Is(err, ErrIllegal) {
		t.Fatalf("ready -> merged: err=%v, want ErrIllegal", err)
	}
}

// Every status the schema allows is in the table, as a source or a target: a status nothing
// can reach or leave is a typo waiting to happen.
func TestTransitionTableCoversEveryStatus(t *testing.T) {
	seen := map[Status]bool{}
	for from, tos := range transitions {
		seen[from] = true
		for _, to := range tos {
			seen[to] = true
		}
	}
	for _, s := range []Status{Draft, Ready, Running, Verifying, Review, Merging, Merged, Split, Blocked, NeedsHuman, Failed, Cancelled} {
		if !seen[s] {
			t.Errorf("%s is unreachable and has no exits", s)
		}
	}
}

func TestMigrationsRunOnce(t *testing.T) {
	p := filepath.Join(t.TempDir(), "swarm.db")
	d, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	steps := append(append([]string{}, migrations...), `CREATE TABLE marker(n INTEGER); INSERT INTO marker VALUES(1);`)
	for range 2 {
		if err := migrate(d.DB, steps); err != nil {
			t.Fatal(err)
		}
	}
	var n, v int
	_ = d.QueryRow(`SELECT count(*) FROM marker`).Scan(&n)
	_ = d.QueryRow(`PRAGMA user_version`).Scan(&v)
	if n != 1 || v != len(steps) {
		t.Fatalf("marker rows=%d version=%d, want 1 and %d", n, v, len(steps))
	}
	d.Close()
}

// The transition table, pinned: a widened edge (merged -> ready, draft -> running) is a
// change to the life cycle and must show up as a failing test, not slip in.
func TestTransitionTableIsPinned(t *testing.T) {
	want := map[Status][]Status{
		Draft:      {Ready, Blocked, Cancelled},
		Ready:      {Running, Cancelled},
		Running:    {Verifying, Ready, Blocked, Cancelled},
		Verifying:  {Review, Merging, Ready, Blocked, Cancelled},
		Review:     {Merging, Ready, Blocked, Cancelled},
		Merging:    {Merged, Verifying, Ready, Blocked, Cancelled},
		Blocked:    {Ready, Split, NeedsHuman, Failed, Cancelled},
		NeedsHuman: {Ready, Cancelled},
	}
	if len(want) != len(transitions) {
		t.Fatalf("table has %d sources, want %d", len(transitions), len(want))
	}
	for from, tos := range want {
		if fmt.Sprint(transitions[from]) != fmt.Sprint(tos) {
			t.Errorf("%s -> %v, want %v", from, transitions[from], tos)
		}
	}
	for _, s := range []Status{Merged, Split, Failed, Cancelled} {
		if len(transitions[s]) != 0 {
			t.Errorf("%s is terminal but has exits %v", s, transitions[s])
		}
	}
	if fmt.Sprint(Settled) != fmt.Sprint([]Status{Merged, Split, Failed, Cancelled, NeedsHuman}) {
		t.Errorf("Settled = %v: a blocked task must keep its run open", Settled)
	}
}

// A provider whose only attempt is still running has outcome NULL; sum(outcome='done') over
// NULLs is NULL, and the report failed to scan it (found live: `swarm report` on a box with
// one running task).
func TestReportWithOnlyARunningAttempt(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	id := seed(t, d, "running")
	if _, err := d.StartAttempt(ctx, id, "claude-x", "m", "local", "a.log"); err != nil {
		t.Fatal(err)
	}
	r, err := d.Report(ctx, 0)
	if err != nil {
		t.Fatalf("report with a running attempt: %v", err)
	}
	if len(r.Providers) != 1 || r.Providers[0].Attempts != 1 || r.Providers[0].Done != 0 {
		t.Fatalf("providers = %+v, want one provider with 1 attempt, 0 done", r.Providers)
	}
}

// The list showed the attempts counter, which a retry zeroes: T101 took four goes and read
// "0 att" (found live, 2026-11). Tries counts the attempts that started.
func TestRecentTasksCountsTriesAcrossARetry(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	id := seed(t, d, "merged")
	other := seed(t, d, "ready")
	for range 2 {
		if _, err := d.StartAttempt(ctx, id, "claude-x", "m", "local", "a.log"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.ExecContext(ctx, `UPDATE tasks SET attempts=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	rows, err := d.RecentTasks(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]TaskRow{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if got[id].Tries != 2 || got[id].Attempts != 0 {
		t.Fatalf("task with two tries after a retry: %+v, want tries 2, attempts 0", got[id])
	}
	if got[other].Tries != 0 {
		t.Fatalf("another task's attempts counted: %+v", got[other])
	}
}

func TestLatestJobRunsOnePerJobWithTheLastLine(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	for i, j := range []string{"a", "b", "a"} {
		id, _ := d.StartJobRun(ctx, j, int64(i), "")
		_ = d.EndJobRun(ctx, id, i, fmt.Sprintf("line one\nrun %d", i), 0)
	}
	r, err := d.LatestJobRuns(ctx)
	if err != nil || len(r) != 2 || r[0].Job != "a" || r[0].Tail != "run 2" || *r[0].Exit != 2 || r[1].Job != "b" {
		t.Fatalf("latest: %+v %v", r, err)
	}
}

func TestCloseLostJobRuns(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	lost, _ := d.StartJobRun(ctx, "a", 1, "")
	done, _ := d.StartJobRun(ctx, "b", 1, "")
	_ = d.EndJobRun(ctx, done, 0, "fine", 0)
	if n, err := d.CloseLostJobRuns(ctx); err != nil || n != 1 {
		t.Fatalf("closed %d (%v), want 1", n, err)
	}
	r, _ := d.JobRuns(ctx, "a", 1)
	if r[0].ID != lost || r[0].EndedAt == 0 || *r[0].Exit != -1 || !strings.Contains(r[0].Tail, "without seeing this run end") {
		t.Fatalf("lost run: %+v", r[0])
	}
}


func TestReportCountsJobRuns(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	for _, exit := range []int{0, 2, 0} {
		id, _ := d.StartJobRun(ctx, "nightly", 1, "")
		_ = d.EndJobRun(ctx, id, exit, "", 0.25)
	}
	_, _ = d.StartJobRun(ctx, "nightly", 2, "held: busy")
	r, err := d.Report(ctx, 0)
	if err != nil || len(r.Jobs) != 1 {
		t.Fatalf("report: %+v %v", r.Jobs, err)
	}
	if j := r.Jobs[0]; j.Runs != 4 || j.Failed != 1 || j.Skipped != 1 || j.CostUSD != 0.75 {
		t.Fatalf("job report: %+v, want 4 runs, 1 failed, 1 skipped, $0.75", j)
	}
}
