package board

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP is the stdio server a worker's CLI launches (`swarm mcp --socket --token`). It holds
// no state: every tool is one call over the socket with the attempt's token.

type empty struct{}

type noteIn struct {
	Kind string `json:"kind" jsonschema:"plan, progress, finding or question"`
	Body string `json:"body" jsonschema:"at most 8192 bytes"`
}

type doneIn struct {
	CommitSHA string `json:"commit_sha" jsonschema:"the commit on your branch to submit"`
	Summary   string `json:"summary" jsonschema:"what changed, one paragraph"`
}

type blockedIn struct {
	Reason   string `json:"reason"`
	Question string `json:"question" jsonschema:"one concrete question the lead can answer"`
}

func result(raw json.RawMessage, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		b, _ := json.Marshal(AsError(err))
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil, nil
}

type listIn struct {
	Status string `json:"status,omitempty" jsonschema:"only this status: ready, running, verifying, merging, merged, blocked; empty for all"`
}

type notesIn struct {
	TaskID int64 `json:"task_id"`
}

// NewSessionMCP is what an ordinary Claude Code session (and its subagents and teammates)
// sees: queue work for the swarm and watch it, never claim to have done it.
func NewSessionMCP(c *Client, token string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "swarm", Version: "0.1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "swarm_status", Description: "Tasks per status, attempts running, dollars spent in 24h. Check before adding work."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, any, error) {
			return result(c.Call("Status", Args{Token: token}))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "swarm_add_task", Description: "Queue a task for the swarm. 15-60 min of work; acceptance must be checkable by the repo's gate; files are the paths it may write (no two concurrent tasks share one). Pass run to group tasks of one change."},
		func(_ context.Context, _ *mcp.CallToolRequest, in AddTaskIn) (*mcp.CallToolResult, any, error) {
			return result(c.Call("AddTask", Args{Token: token, Task: &in}))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "swarm_list_tasks", Description: "Newest 50 tasks, optionally one status. merged is the only status that means done."},
		func(_ context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, any, error) {
			return result(c.Call("ListTasks", Args{Token: token, Status: in.Status}))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "swarm_task_notes", Description: "Why a task is where it is: gate failures, worker plans and questions, newest first."},
		func(_ context.Context, _ *mcp.CallToolRequest, in notesIn) (*mcp.CallToolResult, any, error) {
			return result(c.Call("TaskNotes", Args{Token: token, TaskID: in.TaskID}))
		})
	return s
}

func NewMCP(c *Client, token string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "board", Version: "0.1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "get_task", Description: "Your task: spec, acceptance, files you own, notes from earlier attempts."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, any, error) {
			return result(c.Call("GetTask", Args{Token: token}))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "post_note", Description: "Post a plan, progress, finding or question on your task."},
		func(_ context.Context, _ *mcp.CallToolRequest, in noteIn) (*mcp.CallToolResult, any, error) {
			return result(c.Call("PostNote", Args{Token: token, Kind: in.Kind, Body: in.Body}))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "report_done", Description: "Submit a commit on your branch to the gate. The gate and a reviewer decide whether it is done."},
		func(_ context.Context, _ *mcp.CallToolRequest, in doneIn) (*mcp.CallToolResult, any, error) {
			return result(c.Call("ReportDone", Args{Token: token, Commit: in.CommitSHA, Summary: in.Summary}))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "report_blocked", Description: "Ask the lead for help with one concrete question. Ends your attempt."},
		func(_ context.Context, _ *mcp.CallToolRequest, in blockedIn) (*mcp.CallToolResult, any, error) {
			return result(c.Call("ReportBlocked", Args{Token: token, Reason: in.Reason, Question: in.Question}))
		})
	return s
}
