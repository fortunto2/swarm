package db

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
