package board

import (
	"context"
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

// OperatorToken reads the profile's operator token, creating it on first use.
func OperatorToken(stateDir string) (string, error) {
	p := filepath.Join(stateDir, OperatorTokenFile)
	if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	tok := newToken()
	return tok, os.WriteFile(p, []byte(tok+"\n"), 0o600)
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
	Run        int64    `json:"run,omitempty"`
	Priority   int      `json:"priority,omitempty"`
}

func (s *Service) AddTask(ctx context.Context, tok string, in AddTaskIn) (map[string]int64, error) {
	id, err := s.lead(tok)
	if err != nil {
		return nil, err
	}
	if in.Title == "" || in.Spec == "" || in.Acceptance == "" {
		return nil, &Error{"incomplete", "title, spec and acceptance are required; acceptance is what the gate can check"}
	}
	if in.Tier == "" {
		in.Tier = "B"
	}
	if in.Tier != "A" && in.Tier != "B" {
		return nil, &Error{"bad_tier", "tier is A (hard) or B (routine)"}
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
		if run, err = s.DB.CreateRun(ctx, "session:"+in.Title, "", base); err != nil {
			return nil, err
		}
	}
	tid, err := s.DB.AddTask(ctx, db.NewTask{RunID: run, Repo: in.Repo, Title: in.Title, Spec: in.Spec,
		Acceptance: in.Acceptance, Tier: in.Tier, Priority: in.Priority, Files: in.Files})
	if err != nil {
		return nil, err
	}
	s.DB.Event(ctx, id.Actor(), tid, "task_added", in)
	s.signal(tid)
	return map[string]int64{"run": run, "task": tid}, nil
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

// ListTasks: newest first, at most 50, optionally one status. Paged on purpose: a list of
// every task ever is a context bill, not information.
func (s *Service) ListTasks(ctx context.Context, tok, status string) ([]TaskRow, error) {
	if _, err := s.lead(tok); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, run_id, repo, title, status, attempts, coalesce(provider,'')
		FROM tasks WHERE ?='' OR status=? ORDER BY id DESC LIMIT 50`, status, status)
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

type StatusView struct {
	Tasks   map[string]int `json:"tasks"`
	SpentUS float64        `json:"spent_usd_24h"`
	Workers int            `json:"attempts_running"`
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
	_ = s.DB.QueryRowContext(ctx, `SELECT coalesce(sum(cost_usd),0) FROM attempts WHERE started_at > unixepoch()-86400`).Scan(&v.SpentUS)
	return v, nil
}

func (s *Service) TaskNotes(ctx context.Context, tok string, task int64) ([]db.NoteRow, error) {
	if _, err := s.lead(tok); err != nil {
		return nil, err
	}
	return s.DB.Notes(ctx, task, "", 10)
}
