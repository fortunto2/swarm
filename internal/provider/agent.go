package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fortunto2/swarm/internal/config"
)

// AgentResult: what a scheduled agent job's run said.
type AgentResult struct {
	Text    string  // the model's final text
	CostUSD float64 // the CLI's own estimate; 0 when it gives none
	Failed  bool    // the CLI reported the run as an error (the text says why)
}

// Agent: a provider as the doer of a scheduled job (`[jobs.<name>] prompt`), in the operator's
// own directory. Unlike a worker it is not confined to a worktree it must not leave: it is the
// operator's job, with the operator's settings and MCP servers. Claude runs with acceptEdits
// and the job's allowed tools (anything else would ask a person who is not there, and is
// refused); Codex runs in its workspace-write sandbox with the network on. A mock runs its
// command with the prompt on stdin and prints the text.
func Agent(id string, p config.Provider, prof config.Profile, allowed []string) (func(ctx context.Context, prompt, dir string) (AgentResult, error), error) {
	model := p.Models["job"]
	if model == "" {
		model = p.Models["B"]
	}
	switch p.Kind {
	case "claude":
		return func(ctx context.Context, prompt, dir string) (AgentResult, error) {
			args := []string{"-p", "--output-format", "json", "--permission-mode", "acceptEdits",
				"--disallowedTools", "Agent,Task,Workflow"}
			if len(allowed) > 0 {
				args = append(args, "--allowedTools", strings.Join(allowed, ","))
			}
			if model != "" {
				args = append(args, "--model", model)
			}
			var extra []string
			if p.ConfigDir != "" {
				extra = append(extra, "CLAUDE_CONFIG_DIR="+config.Expand(p.ConfigDir))
			}
			out, err := runCLI(ctx, p, prof, "claude", args, extra, prompt, dir)
			if err != nil {
				return AgentResult{}, err
			}
			var r struct {
				IsError bool    `json:"is_error"`
				Result  string  `json:"result"`
				Cost    float64 `json:"total_cost_usd"`
			}
			if err := json.Unmarshal([]byte(lastLine(out)), &r); err != nil {
				return AgentResult{}, fmt.Errorf("claude: no result: %s", lastLine(out))
			}
			return AgentResult{Text: r.Result, CostUSD: r.Cost, Failed: r.IsError}, nil
		}, nil
	case "codex":
		return func(ctx context.Context, prompt, dir string) (AgentResult, error) {
			args := []string{"exec", "--json", "--sandbox", "workspace-write", "-C", dir,
				"-c", `approval_policy="never"`, "-c", "sandbox_workspace_write.network_access=true", "--skip-git-repo-check"}
			if model != "" {
				args = append(args, "-m", model)
			}
			args = append(args, "-")
			var extra []string
			home := p.Home
			if home == "" {
				home = p.ConfigDir
			}
			if home != "" {
				extra = append(extra, "CODEX_HOME="+config.Expand(home))
			}
			out, err := runCLI(ctx, p, prof, "codex", args, extra, prompt, dir)
			if err != nil {
				return AgentResult{}, err
			}
			res := AgentResult{}
			for _, line := range strings.Split(out, "\n") {
				var e struct {
					Type string `json:"type"`
					Item struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"item"`
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal([]byte(line), &e) != nil {
					continue
				}
				switch {
				case e.Type == "item.completed" && e.Item.Type == "agent_message":
					res.Text = e.Item.Text
				case e.Type == "turn.failed":
					res.Text, res.Failed = e.Error.Message, true
				}
			}
			return res, nil
		}, nil
	case "mock":
		if len(p.Command) == 0 {
			return nil, fmt.Errorf("provider %s: a mock needs command", id)
		}
		return func(ctx context.Context, prompt, dir string) (AgentResult, error) {
			out, err := runCLI(ctx, p, prof, p.Command[0], p.Command[1:], nil, prompt, dir)
			if err != nil {
				return AgentResult{}, err
			}
			// the mock's protocol: a last line FAILED is the run reported as an error
			return AgentResult{Text: out, Failed: lastLine(out) == "FAILED"}, nil
		}, nil
	}
	return nil, errors.New("provider " + id + ": kind " + p.Kind + " cannot run a job")
}
