package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/envx"
	"github.com/fortunto2/swarm/internal/shellx"
)

// Reviewer: a provider as a read-only reader of a change (internal/review). The prompt goes on
// stdin; the result is the model's final text. It never edits: Claude has only Read, Grep and
// Glob, loads the operator's user settings and no others (the checkout is the author's: its
// .claude/, CLAUDE.md and .mcp.json must not configure the reader, as they could run hooks
// with the account), no skills, and deny rules on deny (state, accounts, keys) so an injected
// instruction cannot quote them into findings the author then reads; Codex runs in its
// read-only sandbox (which reads the whole disk: prefer a Claude reviewer where that matters).
// A mock runs its command with the prompt on stdin. The process gets its own group, killed
// with the review's context, and a killed reviewer's partial output is an error, not a verdict.
func Reviewer(id string, p config.Provider, prof config.Profile, deny []string) (func(ctx context.Context, prompt, dir string) (string, error), error) {
	run := func(ctx context.Context, name string, args []string, extra []string, prompt, dir string) (string, error) {
		return runCLI(ctx, p, prof, name, args, extra, prompt, dir)
	}
	model := p.Models["review"]
	if model == "" {
		model = p.Models["A"] // the strongest tier the account serves
	}
	switch p.Kind {
	case "claude":
		return func(ctx context.Context, prompt, dir string) (string, error) {
			args := []string{"-p", "--output-format", "json", "--strict-mcp-config",
				"--tools", "Read,Grep,Glob", "--allowedTools", "Read,Grep,Glob",
				"--disallowedTools", "Edit,Write,NotebookEdit,Bash,Agent,Task,Workflow,WebFetch,WebSearch",
				"--permission-mode", "default", "--setting-sources", "user", "--disable-slash-commands",
				"--settings", WorkerSettings(Attempt{Deny: deny})}
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

// runCLI runs a provider's CLI once: prompt on stdin, in dir, with the provider's resolved env
// over a clean base, in its own process group killed with ctx. A killed run's output is an
// error, never a result.
func runCLI(ctx context.Context, p config.Provider, prof config.Profile, name string, args, extra []string, prompt, dir string) (string, error) {
	vals, err := ResolveEnv(ctx, p.Env, prof)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(append(envx.Clean(), vals...), extra...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = shellx.WaitDelay // a child holding stdout cannot hold the caller past it
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s: %w", name, ctx.Err())
	}
	if err != nil && out.Len() == 0 {
		return "", fmt.Errorf("%s: %w: %s", name, err, lastLine(errb.String()))
	}
	return out.String(), nil
}
