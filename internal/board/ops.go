package board

import (
	"context"
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
)

// One table of operations. The socket (RPC) and the MCP servers are generated from it, so
// an operation is written once: its name, who may call it, its input type and its handler.

type op struct {
	name   string
	desc   string
	role   string // "worker" or "lead"
	schema *jsonschema.Schema
	run    func(s *Service, ctx context.Context, tok string, raw json.RawMessage) (any, error)
}

func def[In any](name, role, desc string, run func(*Service, context.Context, string, In) (any, error)) op {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic("board: schema for " + name + ": " + err.Error())
	}
	return op{name: name, desc: desc, role: role, schema: schema,
		run: func(s *Service, ctx context.Context, tok string, raw json.RawMessage) (any, error) {
			var in In
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &in); err != nil {
					return nil, &Error{"bad_input", err.Error()}
				}
			}
			return run(s, ctx, tok, in)
		}}
}

type empty struct{}

type noteIn struct {
	Kind string `json:"kind" jsonschema:"plan, progress, finding or question"`
	Body string `json:"body" jsonschema:"at most 8192 bytes"`
}

type doneIn struct {
	CommitSHA string `json:"commit_sha" jsonschema:"the commit on your branch to submit"`
	Summary   string `json:"summary,omitempty" jsonschema:"what changed, one paragraph"`
}

type blockedIn struct {
	Reason   string `json:"reason"`
	Question string `json:"question" jsonschema:"one concrete question the lead can answer"`
}

type listIn struct {
	Status string `json:"status,omitempty" jsonschema:"only this status: ready, running, verifying, merging, merged, blocked; empty for all"`
}

type notesIn struct {
	TaskID int64 `json:"task_id"`
}

var ok = map[string]bool{"ok": true}

var ops = []op{
	def("get_task", "worker", "Your task: spec, acceptance, files you own, notes from earlier attempts.",
		func(s *Service, ctx context.Context, tok string, _ empty) (any, error) { return s.GetTask(ctx, tok) }),
	def("post_note", "worker", "Post a plan, progress, finding or question on your task.",
		func(s *Service, ctx context.Context, tok string, in noteIn) (any, error) {
			return ok, s.PostNote(ctx, tok, in.Kind, in.Body)
		}),
	def("report_done", "worker", "Submit a commit on your branch to the gate. The gate and a reviewer decide whether it is done; end your attempt after this.",
		func(s *Service, ctx context.Context, tok string, in doneIn) (any, error) {
			return ok, s.ReportDone(ctx, tok, in.CommitSHA, in.Summary)
		}),
	def("report_blocked", "worker", "Ask the lead for help with one concrete question. Ends your attempt.",
		func(s *Service, ctx context.Context, tok string, in blockedIn) (any, error) {
			return ok, s.ReportBlocked(ctx, tok, in.Reason, in.Question)
		}),
	def("gate_spec", "worker", "The checks your submission will face (gate commands, base, owned files). `swarm gate` runs them for you.",
		func(s *Service, ctx context.Context, tok string, _ empty) (any, error) { return s.GateSpec(ctx, tok) }),
	def("swarm_status", "lead", "Tasks per status, attempts running, dollars spent in 24h. Check before adding work.",
		func(s *Service, ctx context.Context, tok string, _ empty) (any, error) { return s.Status(ctx, tok) }),
	def("swarm_add_task", "lead", "Queue a task for the swarm. 15-60 min of work; acceptance must be checkable by the repo's gate; files are the paths it may write (no two concurrent tasks share one); needs lists task ids that must merge first. Pass run to group tasks of one change.",
		func(s *Service, ctx context.Context, tok string, in AddTaskIn) (any, error) {
			return s.AddTask(ctx, tok, in)
		}),
	def("swarm_list_tasks", "lead", "Newest 50 tasks, optionally one status. merged is the only status that means done.",
		func(s *Service, ctx context.Context, tok string, in listIn) (any, error) {
			return s.ListTasks(ctx, tok, in.Status)
		}),
	def("swarm_task_notes", "lead", "Why a task is where it is: gate failures, worker plans and questions, newest first.",
		func(s *Service, ctx context.Context, tok string, in notesIn) (any, error) {
			return s.TaskNotes(ctx, tok, in.TaskID)
		}),
}

func opNamed(name string) (op, bool) {
	for _, o := range ops {
		if o.name == name {
			return o, true
		}
	}
	return op{}, false
}

// Do runs one operation for a token, enforcing the operation's role.
func (s *Service) Do(ctx context.Context, name, tok string, raw json.RawMessage) (any, error) {
	o, found := opNamed(name)
	if !found {
		return nil, &Error{"unknown_op", name}
	}
	id, err := s.who(tok)
	if err != nil {
		return nil, err
	}
	if id.Role != o.role {
		return nil, &Error{"forbidden", name + " is for the " + o.role}
	}
	return o.run(s, ctx, tok, raw)
}
