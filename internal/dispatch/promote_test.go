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

// A draft whose dependency can no longer merge is blocked with the reason, instead of
// waiting forever and holding its run open.
func TestDraftWithDeadDependencyIsBlocked(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	run, _ := d.CreateRun(ctx, "s", "", "abc")
	a, _ := d.AddTask(ctx, db.NewTask{RunID: run, Repo: "r", Title: "A", Spec: "s", Acceptance: "a", Tier: "B"})
	b, _ := d.AddTask(ctx, db.NewTask{RunID: run, Repo: "r", Title: "B", Spec: "s", Acceptance: "a", Tier: "B", Needs: []int64{a}})
	if _, err := d.ExecContext(ctx, `UPDATE tasks SET status='failed' WHERE id=?`, a); err != nil {
		t.Fatal(err)
	}
	dm := &Daemon{Cfg: &config.Config{}, DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := dm.promote(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetTask(ctx, b)
	if got.Status != db.Blocked {
		t.Fatalf("B is %s, want blocked", got.Status)
	}
	notes, _ := d.Notes(ctx, b, "review", 1)
	if len(notes) != 1 || notes[0].Body != "dependency T1 will not merge" {
		t.Fatalf("notes: %+v", notes)
	}
}
