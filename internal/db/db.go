// Package db is the only writer to the profile's SQLite. Agents never open it; they go
// through the board MCP, which calls into swarmd, which calls this.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// migrations: append-only. Step i runs once, when PRAGMA user_version is i; the version
// then becomes i+1. Step 0 is the base schema (idempotent, so databases made before
// versioning adopt it). A later change is a new step, never an edit of an old one.
var migrations = []string{
	schema,
	// 1: each task has its own base, the main it was branched from or last rebased onto.
	// The run's base is wrong for a task that starts after others merged: its worktree would
	// miss their code, and its diff would claim their files as its own.
	`ALTER TABLE tasks ADD COLUMN base_commit TEXT;`,
	// 2: the worker's process group, so a restart after a crash can stop orphaned workers
	`ALTER TABLE attempts ADD COLUMN pid INTEGER;`,
	// 3: what a task needs from the machine it runs on (node labels, comma-separated), and
	// the nodes that dial in, as last seen, so status can show them without a daemon
	`ALTER TABLE tasks ADD COLUMN runs_on TEXT;
	CREATE TABLE IF NOT EXISTS nodes(name TEXT PRIMARY KEY, seen_at INTEGER NOT NULL, info TEXT NOT NULL DEFAULT '{}');`,
	// 4: step 3's CREATE was a no-op: the base schema has had a nodes table (id, state,
	// last_seen, last_error) since phase 0. What status shows about a node goes there.
	`ALTER TABLE nodes ADD COLUMN info TEXT NOT NULL DEFAULT '{}';`,
	// 5: scheduled jobs: the last scheduled minute each one ran for (or was registered at, so
	// a new job does not fire at once), and every run with its exit and output tail
	`CREATE TABLE IF NOT EXISTS jobs(name TEXT PRIMARY KEY, last_due INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS job_runs(id INTEGER PRIMARY KEY, job TEXT NOT NULL, due_at INTEGER NOT NULL,
	  started_at INTEGER NOT NULL, ended_at INTEGER, exit INTEGER, skipped TEXT, tail TEXT);
	CREATE INDEX IF NOT EXISTS job_runs_job ON job_runs(job, id);`,
	// 6: an agent job's cost, as its CLI estimated it
	`ALTER TABLE job_runs ADD COLUMN cost_usd REAL;`,
	// 7: an attempt can end 'restart': the daemon was restarted under it, which is neither
	// the model's fault nor a crash of the worker. SQLite cannot widen a CHECK in place.
	`CREATE TABLE attempts_new(
	  id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL, provider TEXT NOT NULL, model TEXT, node TEXT,
	  started_at INTEGER, ended_at INTEGER,
	  outcome TEXT CHECK(outcome IN('done','gate_fail','review_fail','timeout','quota','crash','blocked','restart')),
	  log_path TEXT, tokens_in INTEGER, tokens_out INTEGER, cost_usd REAL, pid INTEGER);
	INSERT INTO attempts_new(id, task_id, provider, model, node, started_at, ended_at, outcome, log_path,
	  tokens_in, tokens_out, cost_usd, pid)
	  SELECT id, task_id, provider, model, node, started_at, ended_at, outcome, log_path,
	  tokens_in, tokens_out, cost_usd, pid FROM attempts;
	DROP TABLE attempts;
	ALTER TABLE attempts_new RENAME TO attempts;`,
}

type DB struct {
	*sql.DB
	// OnMove, when set, hears every committed status transition (hooks, a board view).
	// Called synchronously after the write: keep it quick, hand slow work off.
	OnMove func(id int64, to Status)
}

// Open applies the PRD §5 connection settings to every pooled connection and migrates.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "busy_timeout(5000)", "foreign_keys(ON)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	sdb, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if err := migrate(sdb, migrations); err != nil {
		sdb.Close()
		return nil, err
	}
	return &DB{DB: sdb}, nil
}

func migrate(sdb *sql.DB, steps []string) error {
	var v int
	if err := sdb.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(steps); i++ {
		tx, err := sdb.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(steps[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Claim atomically moves one ready task to running. Exactly one caller wins; the rest
// get ErrTaken and move on to the next task.
func (d *DB) Claim(ctx context.Context, taskID int64, provider, model, worker, node string, lease time.Duration) error {
	return d.Move(ctx, "swarmd", taskID, []Status{Ready}, Running, Patch{
		Provider: &provider, Model: &model, Worker: &worker, Node: &node,
		LeaseUntil: P(time.Now().Add(lease).Unix()),
	})
}

// StatusCounts: tasks per status, all runs.
func (d *DB) StatusCounts(ctx context.Context) (map[string]int, error) {
	rows, err := d.QueryContext(ctx, `SELECT status, count(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}
