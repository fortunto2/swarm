package board

import (
	"context"
	"encoding/json"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
)

// The daemon side: net/rpc with the JSON codec on a unix socket. Only `swarm mcp` (one per
// worker) and `swarm report` connect; both carry the token on every call.

type Args struct {
	Token    string     `json:"token"`
	Kind     string     `json:"kind,omitempty"`
	Body     string     `json:"body,omitempty"`
	Commit   string     `json:"commit,omitempty"`
	Summary  string     `json:"summary,omitempty"`
	Reason   string     `json:"reason,omitempty"`
	Question string     `json:"question,omitempty"`
	Task     *AddTaskIn `json:"task,omitempty"`
	Status   string     `json:"status,omitempty"`
	TaskID   int64      `json:"task_id,omitempty"`
}

// Reply carries either a result or a board error, so codes survive the transport.
type Reply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Err    *Error          `json:"err,omitempty"`
}

type RPC struct{ S *Service }

func reply(r *Reply, v any, err error) error {
	if err != nil {
		r.Err = AsError(err)
		return nil
	}
	r.Result, _ = json.Marshal(v)
	return nil
}

func (x *RPC) GetTask(a Args, r *Reply) error {
	v, err := x.S.GetTask(context.Background(), a.Token)
	return reply(r, v, err)
}

func (x *RPC) PostNote(a Args, r *Reply) error {
	return reply(r, map[string]bool{"ok": true}, x.S.PostNote(context.Background(), a.Token, a.Kind, a.Body))
}

func (x *RPC) ReportDone(a Args, r *Reply) error {
	err := x.S.ReportDone(context.Background(), a.Token, a.Commit, a.Summary)
	return reply(r, map[string]string{"ok": "submitted to the gate; you are done with this attempt"}, err)
}

func (x *RPC) ReportBlocked(a Args, r *Reply) error {
	err := x.S.ReportBlocked(context.Background(), a.Token, a.Reason, a.Question)
	return reply(r, map[string]string{"ok": "the lead will answer; end this attempt"}, err)
}

func (x *RPC) AddTask(a Args, r *Reply) error {
	if a.Task == nil {
		return reply(r, nil, &Error{"incomplete", "task is required"})
	}
	v, err := x.S.AddTask(context.Background(), a.Token, *a.Task)
	return reply(r, v, err)
}

func (x *RPC) ListTasks(a Args, r *Reply) error {
	v, err := x.S.ListTasks(context.Background(), a.Token, a.Status)
	return reply(r, v, err)
}

func (x *RPC) Status(a Args, r *Reply) error {
	v, err := x.S.Status(context.Background(), a.Token)
	return reply(r, v, err)
}

func (x *RPC) TaskNotes(a Args, r *Reply) error {
	v, err := x.S.TaskNotes(context.Background(), a.Token, a.TaskID)
	return reply(r, v, err)
}

// Serve listens on the socket until ctx ends. The socket is 0600: same user only.
func Serve(ctx context.Context, sock string, s *Service) error {
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	_ = os.Chmod(sock, 0o600)
	srv := rpc.NewServer()
	if err := srv.RegisterName("Board", &RPC{S: s}); err != nil {
		return err
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go srv.ServeCodec(jsonrpc.NewServerCodec(c))
	}
}

// Client is the worker side of the socket.
type Client struct{ c *rpc.Client }

func Dial(sock string) (*Client, error) {
	c, err := jsonrpc.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	return &Client{c}, nil
}

func (c *Client) Close() error { return c.c.Close() }

// Call returns the result JSON, or the board error as a Go error.
func (c *Client) Call(method string, a Args) (json.RawMessage, error) {
	var r Reply
	if err := c.c.Call("Board."+method, a, &r); err != nil {
		return nil, err
	}
	if r.Err != nil {
		return nil, r.Err
	}
	return r.Result, nil
}
