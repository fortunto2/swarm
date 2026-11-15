package board

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fortunto2/swarm/internal/db"
)

func TestWorkerTokenCannotUseSessionTools(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := &Service{DB: d, BaseFor: func(string) (string, error) { return "abc", nil }}
	worker := s.Issue(Identity{TaskID: 1, Role: "worker"})
	ctx := context.Background()
	if _, err := s.AddTask(ctx, worker, AddTaskIn{Repo: "r", Title: "t", Spec: "s", Acceptance: "a"}); AsError(err).Code != "forbidden" {
		t.Fatalf("worker added a task: %v", err)
	}
	if _, err := s.Status(ctx, "not-a-token"); AsError(err).Code != "bad_token" {
		t.Fatalf("unknown token: %v", err)
	}
	op, err := OperatorToken(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.IssueFixed(op, Identity{Role: "lead"})
	if _, err := s.AddTask(ctx, op, AddTaskIn{Repo: "r", Title: "t", Spec: "s", Acceptance: "a"}); err != nil {
		t.Fatalf("operator could not add: %v", err)
	}
	if _, err := s.AddTask(ctx, op, AddTaskIn{Repo: "r", Title: "t", Spec: "s"}); AsError(err).Code != "incomplete" {
		t.Fatalf("task without acceptance accepted: %v", err)
	}
}
