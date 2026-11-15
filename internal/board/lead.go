package board

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fortunto2/swarm/internal/db"
)

// The session side of the board: any Claude Code session, its subagents and its agent-team
// teammates reach the swarm through the same MCP server, registered once
// (`claude mcp add swarm -- swarm mcp --profile <dir>`). They queue work and watch it; the
// gate still decides what is done. Identity is the operator token in the profile's state,
// readable by the profile's owner only.

const OperatorTokenFile = "operator.token"

// OperatorToken reads the profile's operator token, creating it on first use. Created with
// O_EXCL (two first starts cannot write two different tokens) and kept at 0600.
func OperatorToken(stateDir string) (string, error) {
	p := filepath.Join(stateDir, OperatorTokenFile)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
		_, werr := f.WriteString(newToken() + "\n")
		cerr := f.Close()
		if werr != nil || cerr != nil {
			_ = os.Remove(p)
			return "", errors.Join(werr, cerr)
		}
	}
	_ = os.Chmod(p, 0o600)
	b, err := os.ReadFile(p)
	tok := strings.TrimSpace(string(b))
	if err != nil || len(tok) < 32 {
		return "", fmt.Errorf("operator token at %s is unreadable or short", p)
	}
	return tok, nil
}

// IssueFixed registers a token the caller already holds (the operator token survives
// daemon restarts; attempt tokens do not).
func (s *Service) IssueFixed(tok string, id Identity) { s.put(tok, id) }

func (s *Service) lead(tok string) (Identity, error) {
	id, err := s.who(tok)
	if err != nil {
		return id, err
	}
	if id.Role != "lead" {
		return id, &Error{"forbidden", "this tool is for the lead or an operator session, not a worker"}
	}
	return id, nil
}

type AddTaskIn struct {
	Repo       string   `json:"repo"`
	Title      string   `json:"title"`
	Spec       string   `json:"spec"`
	Acceptance string   `json:"acceptance"`
	Tier       string   `json:"tier,omitempty"`
	Files      []string `json:"files,omitempty"`
	Needs      []int64  `json:"needs,omitempty"`
	Topic      string   `json:"topic,omitempty" jsonschema:"a harness topic id this run serves"`
	Run        int64    `json:"run,omitempty"`
	Priority   int      `json:"priority,omitempty"`
	RunsOn     []string `json:"runs_on,omitempty" jsonschema:"node labels the machine must have (e.g. vpc); empty = any node"`
}

func (s *Service) AddTask(ctx context.Context, tok string, in AddTaskIn) (map[string]int64, error) {
	id, err := s.lead(tok)
	if err != nil {
		return nil, err
	}
	if in.Title == "" || in.Spec == "" || in.Acceptance == "" {
		return nil, &Error{"incomplete", "title, spec and acceptance are required; acceptance is what the gate can check"}
	}
	if len(in.Files) == 0 {
		if s.Strict {
			return nil, &Error{"no_files", `files are required: the paths the task may write (dir/ for a directory, "**" for the whole repo, said on purpose)`}
		}
		in.Files = []string{"**"} // trusted: the whole repo, and it runs alone (it overlaps everything)
	}
	if in.Tier == "" {
		in.Tier = "B"
	}
	if in.Tier != "A" && in.Tier != "B" {
		return nil, &Error{"bad_tier", "tier is A (hard) or B (routine)"}
	}
	for _, l := range in.RunsOn {
		if s.Labels == nil || !s.Labels()[l] {
			return nil, &Error{"bad_runs_on", "no node has the label " + l + ": it would never start (labels are [nodes.<id>].labels)"}
		}
	}
	run := in.Run
	if run == 0 {
		if s.BaseFor == nil {
			return nil, &Error{"internal", "no repo resolver"}
		}
		base, err := s.BaseFor(in.Repo)
		if err != nil {
			return nil, &Error{"bad_repo", err.Error()}
		}
		if run, err = s.DB.CreateRun(ctx, "session:"+in.Title, in.Topic, base); err != nil {
			return nil, err
		}
	}
	tid, err := s.DB.AddTask(ctx, db.NewTask{RunID: run, Repo: in.Repo, Title: in.Title, Spec: in.Spec,
		Acceptance: in.Acceptance, Tier: in.Tier, Priority: in.Priority, Files: in.Files, Needs: in.Needs, RunsOn: in.RunsOn})
	if err != nil {
		return nil, &Error{"bad_task", err.Error()}
	}
	s.DB.Event(ctx, id.Actor(), tid, "task_added", in)
	s.signal(tid)
	return map[string]int64{"run": run, "task": tid}, nil
}

// ListTasks: newest first, at most 50, optionally one status. Paged on purpose: a list of
// every task ever is a context bill, not information.
func (s *Service) ListTasks(ctx context.Context, tok, status string) ([]db.TaskRow, error) {
	if _, err := s.lead(tok); err != nil {
		return nil, err
	}
	return s.DB.RecentTasks(ctx, status, 50)
}

type StatusView struct {
	Tasks     map[string]int `json:"tasks"`
	SpentUS   float64        `json:"spent_usd_24h"`
	Workers   int            `json:"attempts_running"`
	Providers any            `json:"providers,omitempty"`
	Nodes     any            `json:"nodes,omitempty"`
}

func (s *Service) Status(ctx context.Context, tok string) (StatusView, error) {
	if _, err := s.lead(tok); err != nil {
		return StatusView{}, err
	}
	counts, err := s.DB.StatusCounts(ctx)
	if err != nil {
		return StatusView{}, err
	}
	v := StatusView{Tasks: counts, Workers: counts["running"]}
	v.SpentUS, _ = s.DB.SpentSince(ctx, 86400)
	if s.Providers != nil {
		v.Providers, _ = s.Providers(ctx)
	}
	if s.Nodes != nil {
		v.Nodes = s.Nodes.View()
	}
	return v, nil
}

func (s *Service) TaskNotes(ctx context.Context, tok string, task int64) ([]db.NoteRow, error) {
	if _, err := s.lead(tok); err != nil {
		return nil, err
	}
	return s.DB.Notes(ctx, task, "", 10)
}

// RetryTask gives a blocked task (or one that needed a person) a fresh start: counters reset,
// back to ready. Whoever retries has read why it stopped; the notes stay.
func (s *Service) RetryTask(ctx context.Context, tok string, task int64, note string) (any, error) {
	id, err := s.lead(tok)
	if err != nil {
		return nil, err
	}
	if n, err := s.DB.UnmergedDeps(ctx, task); err != nil || n > 0 {
		return nil, &Error{"deps_not_merged", "a dependency of this task has not merged; retry or cancel that one first"}
	}
	zero := 0
	if err := s.DB.Move(ctx, id.Actor(), task, []db.Status{db.Blocked, db.NeedsHuman}, db.Ready,
		db.Patch{Attempts: &zero, InfraFailures: &zero}); err != nil {
		return nil, &Error{"not_retryable", "only a blocked task or one waiting for a person can be retried"}
	}
	s.DB.Event(ctx, id.Actor(), task, "retried", nil) // per-retry counters (rebase conflicts) start here
	if t, err := s.DB.GetTask(ctx, task); err == nil {
		_ = s.DB.ReopenRun(ctx, t.RunID)
		// kind review: the next attempt's prompt carries the newest review notes (found live: a
		// worker's question was answered by hand in its worktree and the retry could not say so)
		if note = strings.TrimSpace(note); note != "" {
			_ = s.DB.Note(ctx, t.RunID, task, id.Actor(), "review", "answer from "+id.Actor()+": "+s.clean(note))
		}
	}
	s.signal(task)
	return map[string]any{"task": task, "status": "ready"}, nil
}

// CancelTask ends a task that will not be done. A running worker is stopped by the daemon.
func (s *Service) CancelTask(ctx context.Context, tok string, task int64) (any, error) {
	id, err := s.lead(tok)
	if err != nil {
		return nil, err
	}
	// not while merging: the fast-forward may already be under way, and a cancelled task
	// must never land on main. It settles within a tick; cancel then, or revert the merge.
	from := []db.Status{db.Draft, db.Ready, db.Running, db.Verifying, db.Review, db.Blocked, db.NeedsHuman}
	if err := s.DB.Move(ctx, id.Actor(), task, from, db.Cancelled, db.Patch{ClearLease: true}); err != nil {
		return nil, &Error{"not_cancellable", "the task is merging or already settled; check again in a moment"}
	}
	s.signal(task)
	return map[string]any{"task": task, "status": "cancelled"}, nil
}

// ClaimFiles widens what a task may write. Two of the first six tasks on nodes blocked on
// exactly this: a test that asserted the behaviour the task was asked to change, outside the
// task's files; the only way out was a new task.
func (s *Service) ClaimFiles(ctx context.Context, tok string, task int64, files []string) (any, error) {
	id, err := s.lead(tok)
	if err != nil {
		return nil, err
	}
	var clean []string
	for _, f := range files {
		if f = strings.TrimSpace(f); f != "" {
			clean = append(clean, f)
		}
	}
	if len(clean) == 0 {
		return nil, &Error{"bad_input", "files: one or more paths or dir/ prefixes"}
	}
	t, err := s.DB.GetTask(ctx, task)
	if err != nil {
		return nil, &Error{"no_task", fmt.Sprintf("no task %d", task)}
	}
	if t.Status == db.Merged || t.Status == db.Cancelled {
		return nil, &Error{"finished", "task is " + string(t.Status) + "; add a new one instead"}
	}
	if err := s.DB.AddClaims(ctx, task, clean); err != nil {
		return nil, err
	}
	s.DB.Event(ctx, id.Actor(), task, "files_claimed", map[string]any{"files": clean})
	all, _ := s.DB.WriteClaims(ctx, task)
	hint := "the next attempt may write them"
	if t.Status == db.Blocked || t.Status == db.NeedsHuman {
		hint = "now retry it: swarm_retry_task"
	}
	return map[string]any{"task": task, "files": all, "next": hint}, nil
}
