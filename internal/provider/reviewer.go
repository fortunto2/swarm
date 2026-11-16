package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/envx"
)

// Reviewer: a provider as a read-only reader of a change (internal/review). The prompt goes on
// stdin; the result is the model's final text. It never edits: Claude gets Read, Grep and Glob
// only, everything else disallowed and nobody to approve a prompt; Codex runs in its read-only
// sandbox. A mock runs its command with the prompt on stdin and returns what it prints.
func Reviewer(id string, p config.Provider, prof config.Profile) (func(ctx context.Context, prompt, dir string) (string, error), error) {
	env := func(ctx context.Context) ([]string, error) {
		vals, err := ResolveEnv(ctx, p.Env, prof)
		if err != nil {
			return nil, err
		}
		return append(envx.Clean(), vals...), nil
	}
	run := func(ctx context.Context, name string, args []string, extra []string, prompt, dir string) (string, error) {
		e, err := env(ctx)
		if err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(e, extra...)
		cmd.Stdin = strings.NewReader(prompt)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err = cmd.Run()
		if err != nil && out.Len() == 0 {
			return "", fmt.Errorf("%s: %w: %s", name, err, lastLine(errb.String()))
		}
		return out.String(), nil
	}
	model := p.Models["review"]
	if model == "" {
		model = p.Models["A"] // the strongest tier the account serves
	}
	switch p.Kind {
	case "claude":
		return func(ctx context.Context, prompt, dir string) (string, error) {
			args := []string{"-p", "--output-format", "json", "--strict-mcp-config",
				"--allowedTools", "Read,Grep,Glob",
				"--disallowedTools", "Edit,Write,NotebookEdit,Bash,Agent,Task,Workflow,WebFetch,WebSearch",
				"--permission-mode", "default"}
			if p.SettingSources != "" {
				args = append(args, "--setting-sources", p.SettingSources)
			}
			if model != "" {
				args = append(args, "--model", model)
			}
			var extra []string
			if p.ConfigDir != "" {
				extra = append(extra, "CLAUDE_CONFIG_DIR="+config.Expand(p.ConfigDir))
			}
			out, err := run(ctx, "claude", args, extra, prompt, dir)
			if err != nil {
				return "", err
			}
			var r struct {
				IsError bool   `json:"is_error"`
				Result  string `json:"result"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(lastLine(out))), &r); err != nil {
				return "", fmt.Errorf("claude: no result: %s", lastLine(out))
			}
			if r.IsError {
				return "", errors.New("claude: " + lastLine(r.Result))
			}
			return r.Result, nil
		}, nil
	case "codex":
		return func(ctx context.Context, prompt, dir string) (string, error) {
			args := []string{"exec", "--json", "--sandbox", "read-only", "-C", dir, "-c", `approval_policy="never"`}
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
			out, err := run(ctx, "codex", args, extra, prompt, dir)
			if err != nil {
				return "", err
			}
			text, failed := "", ""
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
					text = e.Item.Text
				case e.Type == "turn.failed":
					failed = e.Error.Message
				}
			}
			if failed != "" {
				return "", errors.New("codex: " + failed)
			}
			return text, nil
		}, nil
	case "mock":
		if len(p.Command) == 0 {
			return nil, fmt.Errorf("provider %s: a mock reviewer needs command", id)
		}
		return func(ctx context.Context, prompt, dir string) (string, error) {
			return run(ctx, p.Command[0], p.Command[1:], nil, prompt, dir)
		}, nil
	}
	return nil, fmt.Errorf("provider %s: kind %q cannot review", id, p.Kind)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
