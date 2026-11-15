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
}

type DB struct{ *sql.DB }

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
	return &DB{sdb}, nil
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
