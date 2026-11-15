// Package db is the only writer to the profile's SQLite. Agents never open it; they go
// through the board MCP, which calls into swarmd, which calls this.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

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
	if _, err := sdb.Exec(schema); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{sdb}, nil
}

var ErrTaken = errors.New("task is not ready (already taken or not yet promoted)")

// Claim atomically moves one ready task to running. Exactly one caller wins; the rest
// get ErrTaken and move on to the next task.
func (d *DB) Claim(ctx context.Context, taskID int64, provider, model, worker, node string, lease time.Duration) error {
	var id int64
	err := d.QueryRowContext(ctx, `
		UPDATE tasks SET status='running', provider=?1, model=?2, worker_id=?3, node=?4,
		  lease_until=unixepoch()+?5, updated_at=unixepoch()
		WHERE id=?6 AND status='ready'
		RETURNING id`, provider, model, worker, node, int64(lease.Seconds()), taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTaken
	}
	return err
}

// StatusCounts: tasks per status for one run, or for all runs when runID is 0.
func (d *DB) StatusCounts(ctx context.Context, runID int64) (map[string]int, error) {
	rows, err := d.QueryContext(ctx, `SELECT status, count(*) FROM tasks WHERE ?1=0 OR run_id=?1 GROUP BY status`, runID)
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
