package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
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
	RunsOn                  sql.NullString // node labels it needs, comma-separated
	Attempts, InfraFailures int
	LeaseUntil, NotBefore   sql.NullInt64
}

const taskCols = `id, run_id, parent_id, repo, title, spec, acceptance, tier, status, priority,
	provider, model, worker_id, node, result_commit, attempts, infra_failures,
	lease_until, not_before, base_commit, runs_on`

func scanTask(r interface{ Scan(...any) error }) (Task, error) {
	var t Task
	err := r.Scan(&t.ID, &t.RunID, &t.ParentID, &t.Repo, &t.Title, &t.Spec, &t.Acceptance, &t.Tier,
		&t.Status, &t.Priority, &t.Provider, &t.Model, &t.Worker, &t.Node,
		&t.ResultCommit, &t.Attempts, &t.InfraFailures, &t.LeaseUntil, &t.NotBefore, &t.Base, &t.RunsOn)
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
	RunsOn                        []string // node labels the machine must have (vpc, gpu)
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
		priority, status, runs_on, created_at, updated_at) VALUES(?, NULLIF(?, 0), ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), unixepoch(), unixepoch())`,
		n.RunID, n.ParentID, n.Repo, n.Title, n.Spec, n.Acceptance, n.Tier, n.Priority, string(status), strings.Join(n.RunsOn, ","))
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
	Attempts int    `json:"attempts"` // failed attempts toward max_attempts; a retry resets it
	Tries    int    `json:"tries"`    // attempts started, ever: what "how many goes did it take" means
	Provider string `json:"provider,omitempty"`
	Node     string `json:"node,omitempty"`    // where its last attempt ran
	RunsOn   string `json:"runs_on,omitempty"` // labels it asked for, comma-separated
	Topic    string `json:"topic,omitempty"`   // the harness topic its run serves (task add --topic)
	Commit   string `json:"commit,omitempty"`  // what it delivered, once merged
}

// RecentTasks: newest first, at most limit, optionally one status.
func (d *DB) RecentTasks(ctx context.Context, status string, limit int) ([]TaskRow, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, run_id, repo, title, status, attempts,
		(SELECT count(*) FROM attempts a WHERE a.task_id = tasks.id), coalesce(provider,''),
		coalesce(node,''), coalesce(runs_on,''),
		coalesce((SELECT topic FROM runs WHERE runs.id = tasks.run_id),''), coalesce(result_commit,'')
		FROM tasks WHERE ?='' OR status=? ORDER BY id DESC LIMIT ?`, status, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskRow
	for rows.Next() {
		var r TaskRow
		if err := rows.Scan(&r.ID, &r.Run, &r.Repo, &r.Title, &r.Status, &r.Attempts, &r.Tries, &r.Provider, &r.Node, &r.RunsOn, &r.Topic, &r.Commit); err != nil {
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

type OpenAttempt struct {
	Pid     int
	Started int64
}

// OpenAttemptPids: process groups of attempts that never ended, on a node, with when they
// started (so a reused pid is not mistaken for the worker).
func (d *DB) OpenAttemptPids(ctx context.Context, node string) (map[int64]OpenAttempt, error) {
	rows, err := d.QueryContext(ctx, `SELECT task_id, pid, coalesce(started_at,0) FROM attempts
		WHERE outcome IS NULL AND pid IS NOT NULL AND coalesce(node,'')=?`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]OpenAttempt{}
	for rows.Next() {
		var id int64
		var o OpenAttempt
		if err := rows.Scan(&id, &o.Pid, &o.Started); err != nil {
			return nil, err
		}
		out[id] = o
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

// EventCountSinceRetry: events of a type for a task since the operator last retried it.
func (d *DB) EventCountSinceRetry(ctx context.Context, taskID int64, typ string) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE task_id=?1 AND type=?2 AND id >
		coalesce((SELECT max(id) FROM events WHERE task_id=?1 AND type='retried'), 0)`, taskID, typ).Scan(&n)
	return n, err
}

// EventCountWith: events of a type for a task whose data mentions a value (a commit).
func (d *DB) EventCountWith(ctx context.Context, taskID int64, typ, value string) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE task_id=? AND type=? AND instr(data, ?) > 0`, taskID, typ, value).Scan(&n)
	return n, err
}

// UnmergedDeps: dependencies of a task that have not merged.
func (d *DB) UnmergedDeps(ctx context.Context, taskID int64) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT count(*) FROM deps JOIN tasks n ON n.id=deps.needs_id
		WHERE deps.task_id=? AND n.status!='merged'`, taskID).Scan(&n)
	return n, err
}

// Delivery: a merged task's id and the commit it delivered.
type Delivery struct {
	Task   int64
	Commit string
}

// DepDeliveries: what a task's merged dependencies in the same repo delivered, oldest first.
func (d *DB) DepDeliveries(ctx context.Context, taskID int64, repo string) ([]Delivery, error) {
	rows, err := d.QueryContext(ctx, `SELECT n.id, n.result_commit FROM deps JOIN tasks n ON n.id=deps.needs_id
		WHERE deps.task_id=? AND n.status='merged' AND n.repo=? AND coalesce(n.result_commit,'')!=''
		ORDER BY n.id`, taskID, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var x Delivery
		if err := rows.Scan(&x.Task, &x.Commit); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ReopenRun: a run closed as done gets a task back in play.
func (d *DB) ReopenRun(ctx context.Context, runID int64) error {
	_, err := d.ExecContext(ctx, `UPDATE runs SET status='running' WHERE id=? AND status='done'`, runID)
	return err
}

// Report: what a period of work says about the swarm, for a person or a reflection pass
// to act on. Numbers only from what was recorded.
type Report struct {
	Since     int64            `json:"since"`
	Providers []ProviderReport `json:"providers"`
	Merged    int              `json:"tasks_merged"`
	FirstTry  int              `json:"merged_first_try"`
	GateFails []Count          `json:"gate_fail_reasons"`
	Blocked   []BlockedTask    `json:"blocked_now"`
	Signals   map[string]int   `json:"signals"` // flaky, warnings, refusals, rebase conflicts, ...
	Jobs      []JobReport      `json:"jobs,omitempty"`
}

// JobReport: one scheduled job over the period.
type JobReport struct {
	Job     string  `json:"job"`
	Runs    int     `json:"runs"`
	Failed  int     `json:"failed"`
	Skipped int     `json:"skipped"`
	CostUSD float64 `json:"cost_usd"`
}

type ProviderReport struct {
	Provider string  `json:"provider"`
	Attempts int     `json:"attempts"`
	Done     int     `json:"done"`
	GateFail int     `json:"gate_fail"`
	Timeout  int     `json:"timeout"`
	Crash    int     `json:"crash"`
	Restart  int     `json:"restart"` // cut off by a daemon restart: not the model's, not a crash
	Blocked  int     `json:"blocked"`
	CostUSD  float64 `json:"cost_usd"`
}

type Count struct {
	What string `json:"what"`
	N    int    `json:"n"`
}

type BlockedTask struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	LastNote string `json:"last_note"`
}

func (d *DB) Report(ctx context.Context, since int64) (Report, error) {
	r := Report{Since: since, Signals: map[string]int{}}
	// IS, not =: a running attempt has outcome NULL, and sum() over NULLs alone is NULL.
	rows, err := d.QueryContext(ctx, `SELECT provider, count(*),
		sum(outcome IS 'done'), sum(outcome IS 'gate_fail'), sum(outcome IS 'timeout'), sum(outcome IS 'crash'),
		sum(outcome IS 'blocked'), sum(outcome IS 'restart'), coalesce(sum(cost_usd),0)
		FROM attempts WHERE started_at >= ? GROUP BY provider ORDER BY provider`, since)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var p ProviderReport
		if err := rows.Scan(&p.Provider, &p.Attempts, &p.Done, &p.GateFail, &p.Timeout, &p.Crash, &p.Blocked, &p.Restart, &p.CostUSD); err != nil {
			rows.Close()
			return r, err
		}
		r.Providers = append(r.Providers, p)
	}
	rows.Close()
	if err := d.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(t.attempts=0),0) FROM tasks t
		WHERE t.status='merged' AND EXISTS (SELECT 1 FROM events e WHERE e.task_id=t.id AND e.type='merged' AND e.at >= ?)`,
		since).Scan(&r.Merged, &r.FirstTry); err != nil {
		return r, err
	}
	rows, err = d.QueryContext(ctx, `SELECT coalesce(json_extract(data,'$.reason'),'?'), count(*) FROM events
		WHERE type='gate_fail' AND at >= ? GROUP BY 1 ORDER BY 2 DESC LIMIT 5`, since)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.What, &c.N); err != nil {
			rows.Close()
			return r, err
		}
		r.GateFails = append(r.GateFails, c)
	}
	rows.Close()
	rows, err = d.QueryContext(ctx, `SELECT t.id, t.title, coalesce((SELECT substr(body,1,300) FROM notes n
		WHERE n.task_id=t.id ORDER BY n.id DESC LIMIT 1),'') FROM tasks t WHERE t.status='blocked' ORDER BY t.id`)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var b BlockedTask
		if err := rows.Scan(&b.ID, &b.Title, &b.LastNote); err != nil {
			rows.Close()
			return r, err
		}
		r.Blocked = append(r.Blocked, b)
	}
	rows.Close()
	jrows, err := d.QueryContext(ctx, `SELECT job, count(*), sum(coalesce(exit,0) != 0 AND skipped IS NULL),
		sum(skipped IS NOT NULL), coalesce(sum(cost_usd),0) FROM job_runs WHERE started_at >= ? GROUP BY job ORDER BY job`, since)
	if err != nil {
		return r, err
	}
	for jrows.Next() {
		var j JobReport
		if err := jrows.Scan(&j.Job, &j.Runs, &j.Failed, &j.Skipped, &j.CostUSD); err != nil {
			jrows.Close()
			return r, err
		}
		r.Jobs = append(r.Jobs, j)
	}
	jrows.Close()
	for _, typ := range []string{"gate_flaky", "gate_warning", "refused", "rebase_conflict", "infra_fail", "provider_cooling", "provider_disabled", "orphan_killed", "review_skipped", "escalated"} {
		var n int
		if err := d.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE type=? AND at >= ?`, typ, since).Scan(&n); err != nil {
			return r, err
		}
		if n > 0 {
			r.Signals[typ] = n
		}
	}
	return r, nil
}

// RunTopic: the harness topic a run serves ("" when none).
func (d *DB) RunTopic(ctx context.Context, runID int64) string {
	var topic string
	_ = d.QueryRowContext(ctx, `SELECT coalesce(topic,'') FROM runs WHERE id=?`, runID).Scan(&topic)
	return topic
}

// Held: the host's last answer to limits.admit, when it was a hold: the reason and since when.
// ok is false when the last recorded answer admitted, or none was ever recorded.
func (d *DB) Held(ctx context.Context) (why string, since int64, ok bool) {
	var typ, data string
	err := d.QueryRowContext(ctx, `SELECT type, coalesce(data,''), at FROM events
		WHERE type IN ('held','admitted') ORDER BY id DESC LIMIT 1`).Scan(&typ, &data, &since)
	if err != nil || typ != "held" {
		return "", 0, false
	}
	var v struct {
		Why string `json:"why"`
	}
	_ = json.Unmarshal([]byte(data), &v)
	return v.Why, since, true
}

// Labels: a task's runs_on as a list.
func (t Task) Labels() []string {
	if !t.RunsOn.Valid || t.RunsOn.String == "" {
		return nil
	}
	return strings.Split(t.RunsOn.String, ",")
}

// NodeSeen records a node that dialed in, with what status should show about it.
func (d *DB) NodeSeen(ctx context.Context, name string, info any) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx, `INSERT INTO nodes(id, state, last_seen, info) VALUES(?, 'ok', unixepoch(), ?)
		ON CONFLICT(id) DO UPDATE SET state='ok', last_seen=excluded.last_seen, info=excluded.info`, name, string(b))
	return err
}

// NodeRow: a node as last seen.
type NodeRow struct {
	Name   string
	SeenAt int64
	Info   json.RawMessage
}

func (d *DB) Nodes(ctx context.Context) ([]NodeRow, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, coalesce(last_seen, 0), info FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeRow
	for rows.Next() {
		var n NodeRow
		var info string
		if err := rows.Scan(&n.Name, &n.SeenAt, &info); err != nil {
			return nil, err
		}
		n.Info = json.RawMessage(info)
		out = append(out, n)
	}
	return out, rows.Err()
}

// AddClaims widens a task's write claims (the lead answering "may I also change this
// file"); the next attempt's prompt and its gate read the new set.
func (d *DB) AddClaims(ctx context.Context, taskID int64, files []string) error {
	for _, f := range files {
		if _, err := d.ExecContext(ctx, `INSERT OR IGNORE INTO file_claims(path, task_id, mode) VALUES(?, ?, 'write')`, f, taskID); err != nil {
			return err
		}
	}
	return nil
}

// JobDue: the last scheduled minute a job ran for (unix), and whether it is known at all.
func (d *DB) JobDue(ctx context.Context, name string) (int64, bool, error) {
	var due int64
	err := d.QueryRowContext(ctx, `SELECT last_due FROM jobs WHERE name=?`, name).Scan(&due)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return due, err == nil, err
}

// SetJobDue records the scheduled minute a job ran for (or was registered at).
func (d *DB) SetJobDue(ctx context.Context, name string, due int64) error {
	_, err := d.ExecContext(ctx, `INSERT INTO jobs(name, last_due) VALUES(?, ?)
		ON CONFLICT(name) DO UPDATE SET last_due=excluded.last_due`, name, due)
	return err
}

// StartJobRun opens a run row; skipped, when set, closes it at once with that reason.
func (d *DB) StartJobRun(ctx context.Context, name string, due int64, skipped string) (int64, error) {
	var sk any
	var ended any
	if skipped != "" {
		sk, ended = skipped, time.Now().Unix()
	}
	r, err := d.ExecContext(ctx, `INSERT INTO job_runs(job, due_at, started_at, ended_at, skipped) VALUES(?, ?, unixepoch(), ?, ?)`,
		name, due, ended, sk)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// EndJobRun closes a run with its exit code, output tail and (agent jobs) cost.
func (d *DB) EndJobRun(ctx context.Context, id int64, exit int, tail string, cost float64) error {
	var c any
	if cost > 0 {
		c = cost
	}
	_, err := d.ExecContext(ctx, `UPDATE job_runs SET ended_at=unixepoch(), exit=?, tail=?, cost_usd=? WHERE id=?`, exit, tail, c, id)
	return err
}

// JobRun: one run of a scheduled job.
type JobRun struct {
	ID        int64  `json:"id"`
	Job       string `json:"job"`
	DueAt     int64  `json:"due_at"`
	StartedAt int64  `json:"started_at"`
	EndedAt   int64  `json:"ended_at,omitempty"` // 0 while it runs
	Exit      *int   `json:"exit,omitempty"`
	Skipped   string `json:"skipped,omitempty"`
	Tail      string `json:"tail,omitempty"`
}

// JobRuns: newest first, one job or all ("").
func (d *DB) JobRuns(ctx context.Context, job string, limit int) ([]JobRun, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, job, due_at, started_at, coalesce(ended_at,0), exit, coalesce(skipped,''), coalesce(tail,'')
		FROM job_runs WHERE ?='' OR job=? ORDER BY id DESC LIMIT ?`, job, job, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRun
	for rows.Next() {
		var r JobRun
		var exit sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Job, &r.DueAt, &r.StartedAt, &r.EndedAt, &exit, &r.Skipped, &r.Tail); err != nil {
			return nil, err
		}
		if exit.Valid {
			e := int(exit.Int64)
			r.Exit = &e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestJobRuns: each job's newest run, its output cut to the last line (for a status view).
func (d *DB) LatestJobRuns(ctx context.Context) ([]JobRun, error) {
	all, err := d.JobRuns(ctx, "", 500)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []JobRun
	for _, r := range all {
		if seen[r.Job] {
			continue
		}
		seen[r.Job] = true
		t := strings.TrimSpace(r.Tail)
		if i := strings.LastIndex(t, "\n"); i >= 0 {
			t = t[i+1:]
		}
		r.Tail = t
		out = append(out, r)
	}
	return out, nil
}

// CloseLostJobRuns: runs a previous daemon started and never saw end (it was killed, or the
// host went down) are closed, so they stop reading as running.
func (d *DB) CloseLostJobRuns(ctx context.Context) (int64, error) {
	r, err := d.ExecContext(ctx, `UPDATE job_runs SET ended_at=unixepoch(), exit=-1,
		tail=coalesce(tail,'')||'[the daemon stopped without seeing this run end]' WHERE ended_at IS NULL`)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
