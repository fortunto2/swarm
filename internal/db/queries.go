package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

type Task struct {
	ID, RunID               int64
	ParentID                sql.NullInt64
	Repo, Title, Spec       string
	Acceptance, Tier        string
	Status                  Status
	Priority                int
	Provider, Model, Worker sql.NullString
	Node, ResultCommit      sql.NullString
	Base                    sql.NullString // the main this task's branch starts from
	Attempts, InfraFailures int
	LeaseUntil, NotBefore   sql.NullInt64
}

const taskCols = `id, run_id, parent_id, repo, title, spec, acceptance, tier, status, priority,
	provider, model, worker_id, node, result_commit, attempts, infra_failures,
	lease_until, not_before, base_commit`

func scanTask(r interface{ Scan(...any) error }) (Task, error) {
	var t Task
	err := r.Scan(&t.ID, &t.RunID, &t.ParentID, &t.Repo, &t.Title, &t.Spec, &t.Acceptance, &t.Tier,
		&t.Status, &t.Priority, &t.Provider, &t.Model, &t.Worker, &t.Node,
		&t.ResultCommit, &t.Attempts, &t.InfraFailures, &t.LeaseUntil, &t.NotBefore, &t.Base)
	return t, err
}

func (d *DB) GetTask(ctx context.Context, id int64) (Task, error) {
	return scanTask(d.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
}

// TasksIn returns tasks in any of the statuses, scheduling order.
func (d *DB) TasksIn(ctx context.Context, statuses ...Status) ([]Task, error) {
	args := make([]any, len(statuses))
	for i, s := range statuses {
		args[i] = string(s)
	}
	rows, err := d.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE status IN (`+placeholders(len(statuses))+`)
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
	Needs                         []int64  // tasks that must merge first; the task starts as draft
}

func (d *DB) AddTask(ctx context.Context, n NewTask) (int64, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	status := Ready
	if len(n.Needs) > 0 {
		status = Draft
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO tasks(run_id, parent_id, repo, title, spec, acceptance, tier,
		priority, status, created_at, updated_at) VALUES(?, NULLIF(?, 0), ?, ?, ?, ?, ?, ?, ?, unixepoch(), unixepoch())`,
		n.RunID, n.ParentID, n.Repo, n.Title, n.Spec, n.Acceptance, n.Tier, n.Priority, string(status))
	if err != nil {
		return 0, err
	}
	id, _ := r.LastInsertId()
	// a run closed before this task arrived is open again: its done event comes later
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='running' WHERE id=? AND status='done'`, n.RunID); err != nil {
		return 0, err
	}
	for _, f := range n.Files {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO file_claims(path, task_id, mode) VALUES(?, ?, 'write')`, f, id); err != nil {
			return 0, err
		}
	}
	// Edges only ever go from a new task to existing ones, so the graph cannot get a cycle:
	// no check needed while this is the only way to add one.
	for _, need := range n.Needs {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)`, need).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, fmt.Errorf("needs task %d, which does not exist", need)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO deps(task_id, needs_id) VALUES(?, ?)`, id, need); err != nil {
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

// SetBase records where a task's new worktree was branched from (no status change). For
// the record only: the gate measures a change from its merge-base with main.
func (d *DB) SetBase(ctx context.Context, id int64, base string) error {
	_, err := d.ExecContext(ctx, `UPDATE tasks SET base_commit=? WHERE id=?`, base, id)
	return err
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
		AND t.status NOT IN (`+settledList+`))
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

// settledList is Settled as an SQL literal list (constants only, never input).
var settledList = func() string {
	q := make([]string, len(Settled))
	for i, s := range Settled {
		q[i] = "'" + string(s) + "'"
	}
	return strings.Join(q, ",")
}()

func (d *DB) DisableProvider(ctx context.Context, id, why string) error {
	_, err := d.ExecContext(ctx, `INSERT INTO providers(id, state, last_error) VALUES(?, 'disabled', ?)
		ON CONFLICT(id) DO UPDATE SET state='disabled', last_error=excluded.last_error`, id, why)
	return err
}

type TaskRow struct {
	ID       int64  `json:"id"`
	Run      int64  `json:"run"`
	Repo     string `json:"repo"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Provider string `json:"provider,omitempty"`
}

// RecentTasks: newest first, at most limit, optionally one status.
func (d *DB) RecentTasks(ctx context.Context, status string, limit int) ([]TaskRow, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, run_id, repo, title, status, attempts, coalesce(provider,'')
		FROM tasks WHERE ?='' OR status=? ORDER BY id DESC LIMIT ?`, status, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskRow
	for rows.Next() {
		var r TaskRow
		if err := rows.Scan(&r.ID, &r.Run, &r.Repo, &r.Title, &r.Status, &r.Attempts, &r.Provider); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SpentSince: dollars recorded for attempts started in the last `seconds`.
func (d *DB) SpentSince(ctx context.Context, seconds int64) (float64, error) {
	var v float64
	err := d.QueryRowContext(ctx, `SELECT coalesce(sum(cost_usd),0) FROM attempts WHERE started_at > unixepoch()-?`, seconds).Scan(&v)
	return v, err
}

// ClaimsInFlight: write claims of tasks a worker holds or the gate and merge queue are
// handling, keyed by task. Two tasks whose claims overlap must not be in flight together.
func (d *DB) ClaimsInFlight(ctx context.Context) (map[int64][]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT c.task_id, c.path FROM file_claims c JOIN tasks t ON t.id=c.task_id
		WHERE c.mode='write' AND t.status IN ('running','verifying','review','merging')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		out[id] = append(out[id], p)
	}
	return out, rows.Err()
}

// ProviderState: the live half of the provider registry (the declared half is swarm.toml).
type ProviderState struct {
	State        string // ok | cooling | disabled
	CoolingUntil int64
	LastError    string
}

// EnsureProviders registers every declared provider. A provider disabled before this start
// is given another chance: whoever restarts the daemon is the signal it may work again.
// Returns the ids re-enabled.
func (d *DB) EnsureProviders(ctx context.Context, ids []string) ([]string, error) {
	var back []string
	for _, id := range ids {
		if _, err := d.ExecContext(ctx, `INSERT OR IGNORE INTO providers(id, state) VALUES(?, 'ok')`, id); err != nil {
			return nil, err
		}
		r, err := d.ExecContext(ctx, `UPDATE providers SET state='ok', cooling_until=NULL WHERE id=? AND state='disabled'`, id)
		if err != nil {
			return nil, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			back = append(back, id)
		}
	}
	return back, nil
}

func (d *DB) ProviderStates(ctx context.Context) (map[string]ProviderState, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, state, coalesce(cooling_until,0), coalesce(last_error,'') FROM providers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ProviderState{}
	for rows.Next() {
		var id string
		var s ProviderState
		if err := rows.Scan(&id, &s.State, &s.CoolingUntil, &s.LastError); err != nil {
			return nil, err
		}
		out[id] = s
	}
	return out, rows.Err()
}

// CoolProvider pauses a provider until a time (a spent budget, later a rate limit).
func (d *DB) CoolProvider(ctx context.Context, id string, until int64, why string) error {
	_, err := d.ExecContext(ctx, `INSERT INTO providers(id, state, cooling_until, last_error) VALUES(?, 'cooling', ?, ?)
		ON CONFLICT(id) DO UPDATE SET state='cooling', cooling_until=excluded.cooling_until, last_error=excluded.last_error`, id, until, why)
	return err
}

// SpentSinceByProvider: dollars per provider for attempts started at or after a time.
func (d *DB) SpentSinceByProvider(ctx context.Context, since int64) (map[string]float64, error) {
	rows, err := d.QueryContext(ctx, `SELECT provider, coalesce(sum(cost_usd),0) FROM attempts WHERE started_at >= ? GROUP BY provider`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var id string
		var v float64
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

func (d *DB) RunningByProvider(ctx context.Context) (map[string]int, error) {
	rows, err := d.QueryContext(ctx, `SELECT provider, count(*) FROM tasks WHERE status='running' AND provider IS NOT NULL GROUP BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// WarmProvider: a cooling provider whose time is up works again.
func (d *DB) WarmProvider(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, `UPDATE providers SET state='ok', cooling_until=NULL WHERE id=? AND state='cooling'`, id)
	return err
}

// AttemptCount: attempts a task has had, of any outcome.
func (d *DB) AttemptCount(ctx context.Context, taskID int64) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE task_id=?`, taskID).Scan(&n)
	return n, err
}

// DraftsWithDeadDeps: drafts with a dependency that can no longer merge, and that dependency.
func (d *DB) DraftsWithDeadDeps(ctx context.Context) (map[int64]int64, error) {
	rows, err := d.QueryContext(ctx, `SELECT t.id, n.id FROM tasks t JOIN deps ON deps.task_id=t.id
		JOIN tasks n ON n.id=deps.needs_id
		WHERE t.status='draft' AND n.status IN ('failed','cancelled','split','needs_human')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, dep int64
		if err := rows.Scan(&id, &dep); err != nil {
			return nil, err
		}
		out[id] = dep
	}
	return out, rows.Err()
}

func (d *DB) EventCount(ctx context.Context, taskID int64, typ string) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE task_id=? AND type=?`, taskID, typ).Scan(&n)
	return n, err
}

// CloseRunIfSettled marks a run done only if, in the same statement, none of its tasks is
// unsettled: a task added between the count and the write keeps the run open.
func (d *DB) CloseRunIfSettled(ctx context.Context, runID int64) (bool, error) {
	r, err := d.ExecContext(ctx, `UPDATE runs SET status='done' WHERE id=? AND status IN ('planning','running')
		AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.run_id=runs.id AND t.status NOT IN (`+settledList+`))`, runID)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// SetAttemptPid records the process group of a task's open attempt, so a daemon that
// restarts after a crash can stop workers its predecessor left running.
func (d *DB) SetAttemptPid(ctx context.Context, logPath string, pid int) error {
	_, err := d.ExecContext(ctx, `UPDATE attempts SET pid=? WHERE log_path=?`, pid, logPath)
	return err
}

// OpenAttemptPids: process groups of attempts that never ended, on a node.
func (d *DB) OpenAttemptPids(ctx context.Context, node string) (map[int64]int, error) {
	rows, err := d.QueryContext(ctx, `SELECT task_id, pid FROM attempts WHERE outcome IS NULL AND pid IS NOT NULL AND coalesce(node,'')=?`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var pid int
		if err := rows.Scan(&id, &pid); err != nil {
			return nil, err
		}
		out[id] = pid
	}
	return out, rows.Err()
}

// Promotable: drafts whose dependencies all merged.
func (d *DB) Promotable(ctx context.Context) ([]int64, error) {
	rows, err := d.QueryContext(ctx, `SELECT id FROM tasks WHERE status='draft' AND NOT EXISTS (
		SELECT 1 FROM deps JOIN tasks n ON n.id=deps.needs_id WHERE deps.task_id=tasks.id AND n.status!='merged')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (d *DB) NoteCount(ctx context.Context, taskID int64) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT count(*) FROM notes WHERE task_id=?`, taskID).Scan(&n)
	return n, err
}
