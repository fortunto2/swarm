package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Status is a task's place in its life cycle. Every change of it goes through Move, which
// checks the transition against the table below and, in the same UPDATE, that the task is
// still where the caller saw it. A writer that lost a race gets ErrStale instead of
// overwriting the winner.
type Status string

const (
	Draft      Status = "draft"       // waits for its dependencies to merge
	Ready      Status = "ready"       // can be claimed
	Running    Status = "running"     // a worker holds it
	Verifying  Status = "verifying"   // submitted; the gate is checking it
	Review     Status = "review"      // gate passed; a reviewer from another family reads it (phase 4)
	Merging    Status = "merging"     // in the merge queue
	Merged     Status = "merged"      // on main: the only status that means done
	Split      Status = "split"       // replaced by its children
	Blocked    Status = "blocked"     // out of attempts, or asked a question: the lead or a person decides
	NeedsHuman Status = "needs_human" // the lead gave up on it
	Failed     Status = "failed"
	Cancelled  Status = "cancelled"
)

var transitions = map[Status][]Status{
	Draft:      {Ready, Cancelled},
	Ready:      {Running, Cancelled},
	Running:    {Verifying, Ready, Blocked, Cancelled},
	Verifying:  {Review, Merging, Ready, Blocked, Cancelled},
	Review:     {Merging, Ready, Blocked, Cancelled},
	Merging:    {Merged, Verifying, Ready, Blocked, Cancelled},
	Blocked:    {Ready, Split, NeedsHuman, Failed, Cancelled},
	NeedsHuman: {Ready, Cancelled},
}

// Status sets, defined once.
var (
	// Active: the dispatcher moves these on its own.
	Active = []Status{Ready, Running, Verifying, Review, Merging}
	// Waiting: a person or the lead moves these.
	Waiting = []Status{Draft, Blocked, NeedsHuman}
	// Settled: a run whose tasks are all settled is done.
	Settled = []Status{Merged, Split, Failed, Cancelled, NeedsHuman}
)

func CanMove(from, to Status) bool { return slices.Contains(transitions[from], to) }

var (
	ErrStale   = errors.New("task is no longer in the expected status")
	ErrIllegal = errors.New("illegal status transition")
)

// ErrTaken is the claim-shaped name of ErrStale: the task was already taken.
var ErrTaken = ErrStale

// Patch: the fields a transition may set alongside the status. Nil means unchanged.
type Patch struct {
	Provider, Model, Worker, Node *string
	ResultCommit, Base            *string
	LeaseUntil                    *int64
	ClearLease                    bool
	Attempts, InfraFailures       *int
	NotBefore                     *int64
}

func (p Patch) sets() ([]string, []any) {
	var cols []string
	var args []any
	add := func(col string, v any) { cols = append(cols, col+"=?"); args = append(args, v) }
	for _, f := range []struct {
		col string
		v   *string
	}{{"provider", p.Provider}, {"model", p.Model}, {"worker_id", p.Worker}, {"node", p.Node}, {"result_commit", p.ResultCommit}, {"base_commit", p.Base}} {
		if f.v != nil {
			add(f.col, *f.v)
		}
	}
	if p.LeaseUntil != nil {
		add("lease_until", *p.LeaseUntil)
	}
	if p.ClearLease {
		cols = append(cols, "lease_until=NULL")
	}
	if p.Attempts != nil {
		add("attempts", *p.Attempts)
	}
	if p.InfraFailures != nil {
		add("infra_failures", *p.InfraFailures)
	}
	if p.NotBefore != nil {
		add("not_before", *p.NotBefore)
	}
	return cols, args
}

// Move changes a task's status from one of `from` to `to`, applying the patch in the same
// statement, and records the transition in the event log under `actor`.
func (d *DB) Move(ctx context.Context, actor string, id int64, from []Status, to Status, p Patch) error {
	for _, f := range from {
		if !CanMove(f, to) {
			return fmt.Errorf("%w: %s -> %s", ErrIllegal, f, to)
		}
	}
	cols, args := p.sets()
	q := "UPDATE tasks SET status=?, updated_at=unixepoch()"
	if len(cols) > 0 {
		q += ", " + strings.Join(cols, ", ")
	}
	args = append([]any{string(to)}, args...)
	q += " WHERE id=? AND status IN (" + placeholders(len(from)) + ")"
	args = append(args, id)
	for _, f := range from {
		args = append(args, string(f))
	}
	r, err := d.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrStale
	}
	d.Event(ctx, actor, id, "status", map[string]string{"to": string(to)})
	return nil
}

// Renew extends a running task's lease without changing its status.
func (d *DB) Renew(ctx context.Context, id, until int64) error {
	_, err := d.ExecContext(ctx, `UPDATE tasks SET lease_until=? WHERE id=? AND status='running'`, until, id)
	return err
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func ptr[T any](v T) *T { return &v }

// P is a helper for building patches inline: db.P("x").
func P[T any](v T) *T { return ptr(v) }
