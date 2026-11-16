package dispatch

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
)

func jobBed(t *testing.T, jobs map[string]config.Job) (*Daemon, string) {
	t.Helper()
	sdb, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sdb.Close() })
	notified := filepath.Join(t.TempDir(), "notified")
	cfg := &config.Config{Dir: t.TempDir(), Jobs: jobs}
	cfg.Profile.Name = "p"
	cfg.Profile.Notify = `printf '%s\n' "$1" >> ` + notified
	d := &Daemon{Cfg: cfg, DB: sdb, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), prepCtx: context.Background()}
	return d, notified
}

func utc(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t
}

func runs(t *testing.T, d *Daemon, job string) []db.JobRun {
	t.Helper()
	d.bg.Wait()
	r, err := d.DB.JobRuns(context.Background(), job, 50)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A new job waits for its first scheduled minute; then it runs once per minute it is due,
// and a stretch the daemon was down for runs once, for the latest minute only.
func TestJobsRunOncePerScheduledMinute(t *testing.T) {
	d, _ := jobBed(t, map[string]config.Job{"hourly": {Cron: "0 * * * *", Run: "echo hi"}})
	ctx := context.Background()
	d.jobsTick(ctx, utc("2026-11-16 10:30:00"))
	if r := runs(t, d, "hourly"); len(r) != 0 {
		t.Fatalf("a new job ran at once: %+v", r)
	}
	d.jobsTick(ctx, utc("2026-11-16 11:00:05"))
	d.bg.Wait()
	d.jobsTick(ctx, utc("2026-11-16 11:00:40")) // same minute, after it ran
	d.jobsTick(ctx, utc("2026-11-16 11:30:00"))
	r := runs(t, d, "hourly")
	if len(r) != 1 || r[0].Exit == nil || *r[0].Exit != 0 || strings.TrimSpace(r[0].Tail) != "hi" ||
		r[0].DueAt != utc("2026-11-16 11:00:00").Unix() {
		t.Fatalf("runs after 11:00: %+v", r)
	}
	d.jobsTick(ctx, utc("2026-11-16 14:05:00")) // down over 12:00 and 13:00
	r = runs(t, d, "hourly")
	if len(r) != 2 || r[0].DueAt != utc("2026-11-16 14:00:00").Unix() {
		t.Fatalf("after a gap: %d runs, newest due %d, want one more for 14:00", len(r), r[0].DueAt)
	}
}

// A failure is told through notify with its exit and last line; a job over its timeout is
// killed (exit 124) and says so.
func TestJobFailuresAreToldAndTimeoutsKill(t *testing.T) {
	d, notified := jobBed(t, map[string]config.Job{
		"boom": {Cron: "* * * * *", Run: "echo working; echo broke >&2; exit 3"},
		"slow": {Cron: "* * * * *", Run: "sleep 30", Timeout: config.Duration{Duration: 300 * time.Millisecond}},
		"fine": {Cron: "* * * * *", Run: "true"},
	})
	ctx := context.Background()
	d.jobsTick(ctx, utc("2026-11-16 10:00:00"))
	d.jobsTick(ctx, utc("2026-11-16 10:01:00"))
	if r := runs(t, d, "boom"); len(r) != 1 || *r[0].Exit != 3 || !strings.Contains(r[0].Tail, "broke") {
		t.Fatalf("boom: %+v", r)
	}
	if r := runs(t, d, "slow"); len(r) != 1 || *r[0].Exit != 124 || !strings.Contains(r[0].Tail, "over its timeout") {
		t.Fatalf("slow: %+v", r)
	}
	b, _ := os.ReadFile(notified)
	if !strings.Contains(string(b), "job boom exit 3: broke") || !strings.Contains(string(b), "job slow exit 124") ||
		strings.Contains(string(b), "job fine") {
		t.Fatalf("notifications:\n%s", b)
	}
}

// A spare job on a host that is not admitting is skipped for that minute, and the skip is
// on record; a job still running is not started twice.
func TestSpareJobsSkipAndRunsDoNotOverlap(t *testing.T) {
	d, _ := jobBed(t, map[string]config.Job{
		"heavy": {Cron: "* * * * *", Run: "true", Spare: true},
		"long":  {Cron: "* * * * *", Run: "sleep 1"},
	})
	d.Cfg.Limits.Admit = "exit 1"
	d.admit = admission{ok: false, why: "a job runs", checked: true}
	ctx := context.Background()
	d.jobsTick(ctx, utc("2026-11-16 10:00:00"))
	d.jobsTick(ctx, utc("2026-11-16 10:01:00"))
	d.jobsTick(ctx, utc("2026-11-16 10:02:00")) // long is still running from 10:01
	r := runs(t, d, "heavy")
	if len(r) != 2 || !strings.Contains(r[0].Skipped, "held: a job runs") {
		t.Fatalf("heavy: %+v", r)
	}
	r = runs(t, d, "long")
	if len(r) != 2 || r[1].Skipped != "" || !strings.Contains(r[0].Skipped, "still going") {
		t.Fatalf("long: %+v, want one run and the next minute skipped while it went", r)
	}
	d.jobsTick(ctx, utc("2026-11-16 10:02:30")) // it ended: the skipped minute is not run back-to-back
	if r := runs(t, d, "long"); len(r) != 2 {
		t.Fatalf("long reran for a minute it skipped: %d rows", len(r))
	}
}

// Right after a restart limits.admit has not answered yet: a spare job waits for that
// answer instead of recording a blind skip and losing its catch-up minute.
func TestSpareJobWaitsForTheFirstAdmitAnswer(t *testing.T) {
	d, _ := jobBed(t, map[string]config.Job{"heavy": {Cron: "0 3 * * *", Run: "true", Spare: true}})
	d.Cfg.Limits.Admit = "true"
	ctx := context.Background()
	d.jobsTick(ctx, utc("2026-11-15 04:00:00"))
	d.jobsTick(ctx, utc("2026-11-16 03:05:00")) // back after 03:00, admit not asked yet
	if r := runs(t, d, "heavy"); len(r) != 0 {
		t.Fatalf("a spare job decided before admit answered: %+v", r)
	}
	d.admit = admission{ok: true, checked: true}
	d.jobsTick(ctx, utc("2026-11-16 03:05:02"))
	if r := runs(t, d, "heavy"); len(r) != 1 || r[0].Skipped != "" || r[0].DueAt != utc("2026-11-16 03:00:00").Unix() {
		t.Fatalf("heavy after the answer: %+v, want its 03:00 run", r)
	}
}
