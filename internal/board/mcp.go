package board

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewMCP serves the operations of one role over stdio, each one a call over the socket with
// the given token. "worker": a worker's board (`swarm mcp --token`). "lead": what an
// ordinary Claude Code session, its subagents and teammates see (`swarm mcp --profile`):
// queue work and watch it, never claim to have done it.
func NewMCP(c *Client, token, role string) *mcp.Server {
	name := "board"
	if role == "lead" {
		name = "swarm"
	}
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0.1"}, nil)
	for _, o := range ops {
		if o.role != role || o.hidden {
			continue
		}
		s.AddTool(&mcp.Tool{Name: o.name, Description: o.desc, InputSchema: o.schema},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return result(c.Do(o.name, token, req.Params.Arguments))
			})
	}
	return s
}

func result(raw json.RawMessage, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		b, _ := json.Marshal(AsError(err))
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
}
