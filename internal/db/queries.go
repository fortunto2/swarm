package db

import (
	"context"
	"database/sql"
	"encoding/json"
)

type Task struct {
	ID, RunID                int64
	ParentID                 sql.NullInt64
	Repo, Title, Spec        string
	Acceptance, Tier, Status string
	Priority                 int
	Provider, Model, Worker  sql.NullString
	Node, Branch, Worktree   sql.NullString
	ResultCommit             sql.NullString
	Attempts, InfraFailures  int
	LeaseUntil, NotBefore    sql.NullInt64
}

const taskCols = `id, run_id, parent_id, repo, title, spec, acceptance, tier, status, priority,
	provider, model, worker_id, node, branch, worktree, result_commit, attempts, infra_failures,
	lease_until, not_before`

func scanTask(r interface{ Scan(...any) error }) (Task, error) {
	var t Task
	err := r.Scan(&t.ID, &t.RunID, &t.ParentID, &t.Repo, &t.Title, &t.Spec, &t.Acceptance, &t.Tier,
		&t.Status, &t.Priority, &t.Provider, &t.Model, &t.Worker, &t.Node, &t.Branch, &t.Worktree,
		&t.ResultCommit, &t.Attempts, &t.InfraFailures, &t.LeaseUntil, &t.NotBefore)
	return t, err
}

func (d *DB) GetTask(ctx context.Context, id int64) (Task, error) {
	return scanTask(d.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
}

// TasksIn returns tasks in any of the statuses, scheduling order.
func (d *DB) TasksIn(ctx context.Context, statuses ...string) ([]Task, error) {
	args := make([]any, len(statuses))
	ph := ""
	for i, s := range statuses {
		args[i] = s
		if i > 0 {
			ph += ","
		}
		ph += "?"
	}
	rows, err := d.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE status IN (`+ph+`)
		ORDER BY priority DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (d *DB) CreateRun(ctx context.Context, spec, topic, base string) (int64, error) {
	r, err := d.ExecContext(ctx, `INSERT INTO runs(spec_path, topic, status, base_commit, created_at)
		VALUES(?, NULLIF(?, ''), 'running', ?, unixepoch())`, spec, topic, base)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

type NewTask struct {
	RunID                         int64
	ParentID                      int64
	Repo, Title, Spec, Acceptance string
	Tier                          string
	Priority                      int
	Files                         []string // write claims
	Status                        string   // "ready" or "draft"
}

func (d *DB) AddTask(ctx context.Context, n NewTask) (int64, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if n.Status == "" {
		n.Status = "ready"
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO tasks(run_id, parent_id, repo, title, spec, acceptance, tier,
		priority, status, created_at, updated_at) VALUES(?, NULLIF(?, 0), ?, ?, ?, ?, ?, ?, ?, unixepoch(), unixepoch())`,
		n.RunID, n.ParentID, n.Repo, n.Title, n.Spec, n.Acceptance, n.Tier, n.Priority, n.Status)
	if err != nil {
		return 0, err
	}
	id, _ := r.LastInsertId()
	for _, f := range n.Files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO file_claims(path, task_id, mode) VALUES(?, ?, 'write')`, f, id); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// WriteClaims of a task; empty means the task declared no files.
func (d *DB) WriteClaims(ctx context.Context, taskID int64) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT path FROM file_claims WHERE task_id=? AND mode='write' ORDER BY path`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Set updates named columns of a task and stamps updated_at. Column names are code, never input.
func (d *DB) Set(ctx context.Context, id int64, cols map[string]any) error {
	q, args := "UPDATE tasks SET updated_at=unixepoch()", []any{}
	for k, v := range cols {
		q += ", " + k + "=?"
		args = append(args, v)
	}
	_, err := d.ExecContext(ctx, q+" WHERE id=?", append(args, id)...)
	return err
}

// Event appends to the audit log; data is JSON-encoded.
func (d *DB) Event(ctx context.Context, actor string, taskID int64, typ string, data any) {
	b, _ := json.Marshal(data)
	_, _ = d.ExecContext(ctx, `INSERT INTO events(at, actor, task_id, type, data) VALUES(unixepoch(), ?, NULLIF(?, 0), ?, ?)`,
		actor, taskID, typ, string(b))
}

func (d *DB) Note(ctx context.Context, runID, taskID int64, author, kind, body string) error {
	if len(body) > 8192 {
		body = body[len(body)-8192:]
	}
	_, err := d.ExecContext(ctx, `INSERT INTO notes(run_id, task_id, author, kind, body, created_at)
		VALUES(?, NULLIF(?, 0), ?, ?, ?, unixepoch())`, runID, taskID, author, kind, body)
	return err
}

type NoteRow struct {
	Author, Kind, Body string
	At                 int64
}

func (d *DB) Notes(ctx context.Context, taskID int64, kind string, limit int) ([]NoteRow, error) {
	rows, err := d.QueryContext(ctx, `SELECT author, kind, body, created_at FROM notes
		WHERE task_id=? AND (?='' OR kind=?) ORDER BY id DESC LIMIT ?`, taskID, kind, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteRow
	for rows.Next() {
		var n NoteRow
		if err := rows.Scan(&n.Author, &n.Kind, &n.Body, &n.At); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (d *DB) StartAttempt(ctx context.Context, taskID int64, provider, model, node, log string) (int64, error) {
	r, err := d.ExecContext(ctx, `INSERT INTO attempts(task_id, provider, model, node, started_at, log_path)
		VALUES(?, ?, ?, ?, unixepoch(), ?)`, taskID, provider, model, node, log)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// EndAttempt closes the open attempt of a task with an outcome (no-op if none is open).
func (d *DB) EndAttempt(ctx context.Context, taskID int64, outcome string) {
	_, _ = d.ExecContext(ctx, `UPDATE attempts SET ended_at=unixepoch(), outcome=? WHERE id=(
		SELECT id FROM attempts WHERE task_id=? AND outcome IS NULL ORDER BY id DESC LIMIT 1)`, outcome, taskID)
}

func (d *DB) RunBase(ctx context.Context, runID int64) (string, error) {
	var b string
	err := d.QueryRowContext(ctx, `SELECT base_commit FROM runs WHERE id=?`, runID).Scan(&b)
	return b, err
}

func (d *DB) SetRunStatus(ctx context.Context, runID int64, status string) error {
	_, err := d.ExecContext(ctx, `UPDATE runs SET status=? WHERE id=?`, status, runID)
	return err
}

// OpenRuns: runs still planning or running, with how many of their tasks are not terminal.
func (d *DB) OpenRuns(ctx context.Context) (map[int64]int, error) {
	rows, err := d.QueryContext(ctx, `SELECT r.id, (SELECT count(*) FROM tasks t WHERE t.run_id=r.id
		AND t.status NOT IN ('merged','split','failed','cancelled','needs_human'))
		FROM runs r WHERE r.status IN ('planning','running')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// SetUsage records what an attempt spent, keyed by its log file.
func (d *DB) SetUsage(ctx context.Context, logPath string, in, out int64, cost float64) {
	_, _ = d.ExecContext(ctx, `UPDATE attempts SET tokens_in=?, tokens_out=?, cost_usd=? WHERE log_path=?`, in, out, cost, logPath)
}
