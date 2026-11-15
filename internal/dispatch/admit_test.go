package dispatch

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
)

// Memory near the admit threshold flips the answer every 15 s; on the box that wrote ten
// held/admitted lines and events an hour and kept resetting "held since". A change is
// recorded once it is answered twice in a row (the very first answer at once); scheduling
// still follows the latest answer.
func TestAdmitRecordsAChangeOnlyWhenItHolds(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	dm := &Daemon{Cfg: &config.Config{Limits: config.Limits{Admit: "true"}}, DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	answers := []bool{false, true, false, true, true, true}
	for _, ok := range answers {
		dm.applyAdmit(ctx, admission{ok: ok, why: "only 2400 MB free", at: time.Now(), checked: true})
		if dm.admitting() != ok {
			t.Fatalf("scheduling did not follow the latest answer (%v)", ok)
		}
	}
	rows, err := d.QueryContext(ctx, `SELECT type FROM events WHERE type IN ('held','admitted') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		got = append(got, s)
	}
	if len(got) != 2 || got[0] != "held" || got[1] != "admitted" {
		t.Fatalf("events = %v, want [held admitted]", got)
	}
}
