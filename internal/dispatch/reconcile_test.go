package dispatch

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
)

// A daemon restart cuts a running attempt off. The task goes back to ready at once, and the
// attempt is recorded as 'restart': not a crash of the worker, not a model failure, so
// `swarm report` does not count it as one and no attempt or infra strike is charged.
func TestReconcileEndsACutOffAttemptAsRestartNotCrash(t *testing.T) {
	sdb, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	ctx := context.Background()
	run, _ := sdb.CreateRun(ctx, "x", "", "base")
	id, _ := sdb.AddTask(ctx, db.NewTask{RunID: run, Repo: "app", Title: "T", Spec: "s", Acceptance: "a", Tier: "B"})
	if err := sdb.Move(ctx, "t", id, []db.Status{db.Ready}, db.Running, db.Patch{}); err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.StartAttempt(ctx, id, "p", "m", "local", "/nonexistent/log"); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Cfg: &config.Config{}, DB: sdb, Node: "local", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := d.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	tk, _ := sdb.GetTask(ctx, id)
	if tk.Status != db.Ready || tk.Attempts != 0 || tk.InfraFailures != 0 {
		t.Fatalf("task = %s attempts=%d infra=%d, want ready 0 0", tk.Status, tk.Attempts, tk.InfraFailures)
	}
	var out string
	if err := sdb.QueryRow(`SELECT outcome FROM attempts WHERE task_id=?`, id).Scan(&out); err != nil || out != "restart" {
		t.Fatalf("attempt outcome = %q (%v), want restart", out, err)
	}
	r, err := sdb.Report(ctx, 0)
	if err != nil || len(r.Providers) != 1 || r.Providers[0].Crash != 0 || r.Providers[0].Restart != 1 {
		t.Fatalf("report = %+v (%v), want 0 crash 1 restart", r.Providers, err)
	}
}
