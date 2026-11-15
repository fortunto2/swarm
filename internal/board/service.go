// Package board is the only door agents have into the system. Identity comes from a
// one-time token swarmd issued for the attempt, so a worker cannot speak as another task
// or as the lead.
package board

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/db"
	"github.com/fortunto2/swarm/internal/gate"
	"github.com/fortunto2/swarm/internal/gitx"
	"github.com/fortunto2/swarm/internal/worktree"
)

type Identity struct {
	TaskID int64
	Role   string // "worker" | "lead"
}

func (i Identity) Actor() string { return fmt.Sprintf("%s:T%d", i.Role, i.TaskID) }

// Error is what a tool returns instead of an exception: a code an agent can branch on and
// a hint it can act on.
type Error struct {
	Code string `json:"error"`
	Hint string `json:"hint"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Hint }

type Service struct {
	DB *db.DB
	// Done is signalled after a state change the dispatcher should act on now, not next tick.
	Done chan<- int64
	// BaseFor gives the current main commit of a profile repo, for a run a session starts.
	BaseFor func(repo string) (string, error)
	// Locate gives a task's worktree and branch (derived, never stored).
	Locate func(t db.Task) worktree.WT
	// Repo gives a profile repo's settings (the gate a worker runs on itself).
	Repo func(name string) (config.Repo, error)
	// Providers gives the provider registry view (declared + live).
	Providers func(ctx context.Context) (any, error)

	mu     sync.Mutex
	tokens map[string]Identity
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Service) Issue(id Identity) string {
	tok := newToken()
	s.put(tok, id)
	return tok
}

func (s *Service) put(tok string, id Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = map[string]Identity{}
	}
	s.tokens[tok] = id
}

func (s *Service) Revoke(tok string) {
	s.mu.Lock()
	delete(s.tokens, tok)
	s.mu.Unlock()
}

func (s *Service) who(tok string) (Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.tokens[tok]
	if !ok {
		return id, &Error{"bad_token", "this token is not live: the attempt ended or never started"}
	}
	return id, nil
}

type TaskView struct {
	ID         int64    `json:"id"`
	Title      string   `json:"title"`
	Spec       string   `json:"spec"`
	Acceptance string   `json:"acceptance"`
	Tier       string   `json:"tier"`
	Branch     string   `json:"branch"`
	Worktree   string   `json:"worktree"`
	Files      []string `json:"files_write"`
	Previous   []string `json:"previous_attempts"`
	Attempt    int      `json:"attempt"`
}

func (s *Service) GetTask(ctx context.Context, tok string) (TaskView, error) {
	id, err := s.who(tok)
	if err != nil {
		return TaskView{}, err
	}
	t, err := s.DB.GetTask(ctx, id.TaskID)
	if err != nil {
		return TaskView{}, err
	}
	files, _ := s.DB.WriteClaims(ctx, t.ID)
	prev, _ := s.DB.Notes(ctx, t.ID, "review", 4)
	wt := s.Locate(t)
	v := TaskView{ID: t.ID, Title: t.Title, Spec: t.Spec, Acceptance: t.Acceptance, Tier: t.Tier,
		Branch: wt.Branch, Worktree: wt.Path, Files: files, Attempt: t.Attempts + 1}
	for _, n := range prev {
		v.Previous = append(v.Previous, n.Body)
	}
	return v, nil
}

var noteKinds = []string{"plan", "progress", "finding", "question"}

func (s *Service) PostNote(ctx context.Context, tok, kind, body string) error {
	id, err := s.who(tok)
	if err != nil {
		return err
	}
	ok := false
	for _, k := range noteKinds {
		ok = ok || k == kind
	}
	if !ok {
		return &Error{"bad_kind", "kind is one of " + strings.Join(noteKinds, ", ")}
	}
	if len(body) > 8192 {
		return &Error{"too_long", "body is at most 8192 bytes; post the gist, not the log"}
	}
	t, err := s.DB.GetTask(ctx, id.TaskID)
	if err != nil {
		return err
	}
	return s.DB.Note(ctx, t.RunID, t.ID, id.Actor(), kind, body)
}

// ReportDone hands the work to the gate. It does not count the task as done.
func (s *Service) ReportDone(ctx context.Context, tok, commit, summary string) error {
	id, err := s.who(tok)
	if err != nil {
		return err
	}
	t, err := s.DB.GetTask(ctx, id.TaskID)
	if err != nil {
		return err
	}
	if t.Status != db.Running {
		return &Error{"not_running", "task is " + string(t.Status) + "; report_done is accepted once per attempt"}
	}
	loc := s.Locate(t)
	wt := loc.Path
	sha, err := gitx.Head(ctx, wt, commit)
	if err != nil {
		return &Error{"no_commit", "commit " + commit + " does not exist in your worktree"}
	}
	if !gitx.IsAncestor(ctx, wt, sha, "refs/heads/"+loc.Branch) {
		return &Error{"wrong_branch", "commit is not on " + loc.Branch + "; commit to your own branch"}
	}
	base, err := s.DB.TaskBase(ctx, t)
	if err != nil {
		return err
	}
	if sha == base || gitx.IsAncestor(ctx, wt, sha, base) {
		return &Error{"no_change", "the commit adds nothing over your branch's base"}
	}
	if err := s.DB.Move(ctx, id.Actor(), t.ID, []db.Status{db.Running}, db.Verifying, db.Patch{ResultCommit: &sha, ClearLease: true}); err != nil {
		if errors.Is(err, db.ErrStale) {
			return &Error{"not_running", "the attempt ended before this report arrived"}
		}
		return err
	}
	s.DB.EndAttempt(ctx, t.ID, "done")
	s.DB.Event(ctx, id.Actor(), t.ID, "report_done", map[string]string{"commit": sha, "summary": summary})
	s.signal(t.ID)
	return nil
}

func (s *Service) ReportBlocked(ctx context.Context, tok, reason, question string) error {
	id, err := s.who(tok)
	if err != nil {
		return err
	}
	if strings.TrimSpace(question) == "" {
		return &Error{"no_question", "ask one concrete question the lead can answer"}
	}
	t, err := s.DB.GetTask(ctx, id.TaskID)
	if err != nil {
		return err
	}
	if err := s.DB.Move(ctx, id.Actor(), t.ID, []db.Status{db.Running}, db.Blocked, db.Patch{ClearLease: true}); err != nil {
		if errors.Is(err, db.ErrStale) {
			return &Error{"not_running", "the attempt ended before this report arrived"}
		}
		return err
	}
	s.DB.EndAttempt(ctx, t.ID, "blocked")
	_ = s.DB.Note(ctx, t.RunID, t.ID, id.Actor(), "question", reason+"\n\nQ: "+question)
	s.DB.Event(ctx, id.Actor(), t.ID, "report_blocked", map[string]string{"reason": reason, "question": question})
	s.signal(t.ID)
	return nil
}

func (s *Service) signal(id int64) {
	if s.Done != nil {
		select {
		case s.Done <- id:
		default:
		}
	}
}

// AsError unwraps a board error for transport; anything else becomes "internal".
func AsError(err error) *Error {
	var be *Error
	if errors.As(err, &be) {
		return be
	}
	return &Error{"internal", err.Error()}
}

// GateSpec is what `swarm gate` needs to run, in a worker's worktree, the same checks the
// daemon will run on its submission.
type GateSpec struct {
	Commands   []string `json:"commands"`
	TimeoutSec int      `json:"timeout_sec"`
	Base       string   `json:"base"`
	Files      []string `json:"files_write"`
	AllowTests bool     `json:"allow_test_changes"`
}

func (s *Service) GateSpec(ctx context.Context, tok string) (GateSpec, error) {
	id, err := s.who(tok)
	if err != nil {
		return GateSpec{}, err
	}
	t, err := s.DB.GetTask(ctx, id.TaskID)
	if err != nil {
		return GateSpec{}, err
	}
	r, err := s.Repo(t.Repo)
	if err != nil {
		return GateSpec{}, err
	}
	base, err := s.DB.TaskBase(ctx, t)
	if err != nil {
		return GateSpec{}, err
	}
	files, _ := s.DB.WriteClaims(ctx, t.ID)
	return GateSpec{Commands: r.Gate, TimeoutSec: int(r.GateTimeout.Seconds()), Base: base, Files: files,
		AllowTests: strings.Contains(t.Spec, gate.AllowTestsMarker)}, nil
}
