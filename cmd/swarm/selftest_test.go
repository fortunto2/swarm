package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fortunto2/swarm/internal/db"
)

// The FAILED line's reason is a verdict, not the worker's chatter: a plan note posted before
// three crashes must not stand in for the crash, and a gate verdict wins over the crash events
// before it.
func TestSelftestFailReasonSkipsChatter(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "swarm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	run, err := d.CreateRun(ctx, "task:x", "", "base")
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.AddTask(ctx, db.NewTask{RunID: run, Repo: "selftest", Title: "x", Spec: "x", Tier: "B"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := d.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Note(ctx, run, id, "worker", "plan", "write the file"); err != nil {
		t.Fatal(err)
	}
	d.Event(ctx, "swarmd", id, "infra_fail", map[string]string{"outcome": "crash", "detail": "worker exited without report_done"})
	if got, want := failReason(ctx, d, task, "p"), "crash: worker exited without report_done"; got != want {
		t.Fatalf("after a plan note and a crash: %q, want %q", got, want)
	}
	if err := d.Note(ctx, run, id, "swarmd", "review", "attempt 1: gate_fail\ngate: exit 1"); err != nil {
		t.Fatal(err)
	}
	if got, want := failReason(ctx, d, task, "p"), "attempt 1: gate_fail; gate: exit 1"; got != want {
		t.Fatalf("after a gate verdict: %q, want %q", got, want)
	}
}
