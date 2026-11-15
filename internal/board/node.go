package board

import (
	"context"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/gate"
)

// The node link: a machine that dials in (`swarm node`) and runs workers there. It speaks
// the same socket protocol as everything else, with the operator's token, through three
// operations: hello (who it is, what it has), poll (wait for work, report liveness) and
// report (what happened). The daemon decides; the node executes and tells.

// NodeHub is the daemon's side of the link.
type NodeHub interface {
	Hello(ctx context.Context, in NodeHello) (NodeWelcome, error)
	Poll(ctx context.Context, in NodePoll) (NodeOrders, error)
	Report(ctx context.Context, in NodeReport) error
	View() any
}

// NodeProtocol: what orders a node understands. A coordinator places new work only on a node
// at its own protocol or later; an older node keeps its running work and gates, and status
// says to upgrade it. 1: nodes (absent in a hello = 1). 2: a start order's Ref names a joined
// base to fetch (taskbase), which a protocol-1 node ignores and then fails the start.
const NodeProtocol = 2

// NodeHello: a node says who it is and what it holds. Live and Worktrees let a daemon that
// restarted, or a node that reconnects, agree on what is still running and what can go.
type NodeHello struct {
	Name     string   `json:"name"`
	Version  string   `json:"version,omitempty"`
	Protocol int      `json:"protocol,omitempty"`
	Root     string   `json:"root"`  // the node's worktree root
	Repos    []string `json:"repos"` // repos it has a copy of
	Live     []string `json:"live,omitempty"`
	// Worktrees by repo and task. A new name: a node of the release before sent bare ids
	// under "worktrees", which this coordinator ignores (it never cleans by id alone), and
	// an older coordinator ignores this field. Neither side fails the other's hello.
	Worktrees []TaskRef `json:"worktree_refs,omitempty"`
}

// TaskRef: a task's worktree on a node, by repo and id. The repo matters: another profile
// sharing the worktree root can hold a worktree with the same id for another repo.
type TaskRef struct {
	Repo string `json:"repo"`
	Task int64  `json:"task"`
}

// RepoView: what a node needs to know about a repo to branch from it and gate on it.
type RepoView struct {
	MainRef     string          `json:"main_ref"`
	Merge       string          `json:"merge"`         // ff or branch: a node fetches delivered branches for its people
	Prefix      string          `json:"branch_prefix"` // where branch delivery puts them
	Gate        []string        `json:"gate"`
	GateTimeout config.Duration `json:"gate_timeout"`
	Reruns      int             `json:"reruns"`
}

// NodeWelcome: the session, this node's accounts as declared by the coordinator (their
// paths and secret references resolve on the node), and worktrees it may delete.
type NodeWelcome struct {
	Session   string                     `json:"session"`
	Providers map[string]config.Provider `json:"providers"`
	Repos     map[string]RepoView        `json:"repos"`
	Cleanup   []TaskRef                  `json:"cleanup_refs,omitempty"` // a new name, as Worktrees
}

// NodePoll waits up to Wait seconds for orders. Admit is the node's own limits.admit
// answer; Live lists the attempts still running there.
type NodePoll struct {
	Session string   `json:"session"`
	Admit   bool     `json:"admit"`
	Why     string   `json:"why,omitempty"`
	Live    []string `json:"live,omitempty"`
	Wait    int      `json:"wait,omitempty"`
	Cool    []Cool   `json:"cool,omitempty"` // its accounts over their limit, read from their limits files there
	Warn    string   `json:"warn,omitempty"` // what fails to stay in step (publish, fetch), while the link holds
}

// Cool: an account over its limit until a time, by the node's reading.
type Cool struct {
	Provider string `json:"provider"`
	Until    int64  `json:"until"`
	Why      string `json:"why"`
}

// NodeOrders: what to do now, and where each repo's main is (a node keeps it fetched, so
// a worker's `swarm gate` finds its base, and fast-forwards its own main when it lands).
type NodeOrders struct {
	Cmds  []NodeCmd         `json:"cmds,omitempty"`
	Mains map[string]string `json:"mains,omitempty"`
}

// NodeCmd kinds: start (an attempt), kill (an attempt), push (the task branch, before its
// report_done is checked), gate (a commit, with the repo's commands), cleanup (a task's
// worktree, after merge or cancel), status (a task of this node moved).
type NodeCmd struct {
	Kind   string `json:"kind"`
	Task   int64  `json:"task"`
	Worker string `json:"worker,omitempty"`
	Repo   string `json:"repo,omitempty"`

	// start
	Base      string          `json:"base,omitempty"`
	Prompt    string          `json:"prompt,omitempty"`
	Provider  string          `json:"provider,omitempty"`
	Model     string          `json:"model,omitempty"`
	Token     string          `json:"token,omitempty"`
	Slot      int             `json:"slot,omitempty"`
	WallClock config.Duration `json:"wall_clock,omitempty"`

	// gate: the commit is fetched from Ref on the coordinator's copy. start: Ref is set when
	// Base is not the coordinator's main (main joined with dependencies' deliveries).
	Commit string `json:"commit,omitempty"`
	Ref    string `json:"ref,omitempty"`

	// status: the task moved; the node runs its own hooks.on_status (a board, a card)
	Status string `json:"status,omitempty"`
	Title  string `json:"title,omitempty"`
}

// NodeReport kinds: started (pid), exited (err, tail of the log), pushed (err), gate
// (verdict), failed (a start or gate that could not run: err).
type NodeReport struct {
	Session string        `json:"session"`
	Kind    string        `json:"kind"`
	Task    int64         `json:"task"`
	Worker  string        `json:"worker,omitempty"`
	Commit  string        `json:"commit,omitempty"`
	Pid     int           `json:"pid,omitempty"`
	Err     string        `json:"err,omitempty"`
	Tail    string        `json:"tail,omitempty"`
	Verdict *gate.Verdict `json:"verdict,omitempty"`
}

func (s *Service) node(tok string) error {
	if _, err := s.lead(tok); err != nil {
		return err
	}
	if s.Nodes == nil {
		return &Error{"no_nodes", "this daemon takes no nodes"}
	}
	return nil
}

func (s *Service) NodeHello(ctx context.Context, tok string, in NodeHello) (NodeWelcome, error) {
	if err := s.node(tok); err != nil {
		return NodeWelcome{}, err
	}
	return s.Nodes.Hello(ctx, in)
}

func (s *Service) NodePoll(ctx context.Context, tok string, in NodePoll) (NodeOrders, error) {
	if err := s.node(tok); err != nil {
		return NodeOrders{}, err
	}
	return s.Nodes.Poll(ctx, in)
}

func (s *Service) NodeReport(ctx context.Context, tok string, in NodeReport) error {
	if err := s.node(tok); err != nil {
		return err
	}
	return s.Nodes.Report(ctx, in)
}
