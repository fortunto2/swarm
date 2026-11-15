package board

import (
	"context"
	"encoding/json"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
)

// The daemon side: one RPC method on a unix socket, dispatching through the op table. Only
// `swarm mcp` and `swarm board` connect; every call carries its token.

type Req struct {
	Op    string          `json:"op"`
	Token string          `json:"token"`
	Input json.RawMessage `json:"input,omitempty"`
}

// Reply carries either a result or a board error, so codes survive the transport.
type Reply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Err    *Error          `json:"err,omitempty"`
}

type RPC struct{ S *Service }

func (x *RPC) Do(r Req, out *Reply) error {
	v, err := x.S.Do(context.Background(), r.Op, r.Token, r.Input)
	if err != nil {
		out.Err = AsError(err)
		return nil
	}
	out.Result, _ = json.Marshal(v)
	return nil
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

// Client is the caller side of the socket.
type Client struct{ c *rpc.Client }

func Dial(sock string) (*Client, error) {
	c, err := jsonrpc.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	return &Client{c}, nil
}

func (c *Client) Close() error { return c.c.Close() }

// Do returns the result JSON, or the board error as a Go error. input may be any value
// that marshals to the operation's input.
func (c *Client) Do(op, token string, input any) (json.RawMessage, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var r Reply
	if err := c.c.Call("Board.Do", Req{Op: op, Token: token, Input: raw}, &r); err != nil {
		return nil, err
	}
	if r.Err != nil {
		return nil, r.Err
	}
	return r.Result, nil
}
