// Package provider turns one attempt into one process of an official CLI. Nothing here
// wraps a subscription's OAuth into an API: each provider runs its own binary.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/fortunto2/swarm/internal/config"
)

// Attempt is everything a worker process needs. Dir holds the prompt, the MCP config and
// the log; it lives in the profile's state, never in the worktree (an agent's `git add -A`
// would commit it).
type Attempt struct {
	TaskID   int64
	N        int
	Worktree string
	Dir      string
	Prompt   string
	Model    string
	Socket   string
	Token    string
	SwarmBin string
	ExtraMCP map[string]config.MCPServer
	Gate     []string // the repo's gate commands; their programs are allowed to run
}

// AllowedTools: with no sandbox and nobody to answer a prompt, a worker may run only what
// its job needs. git push is not in the list on purpose.
func AllowedTools(a Attempt, extra []string) []string {
	base := []string{"mcp__board__*", "Read", "Write", "Edit", "Glob", "Grep",
		"Bash(git add:*)", "Bash(git commit:*)", "Bash(git status:*)", "Bash(git diff:*)",
		"Bash(git log:*)", "Bash(git rev-parse:*)", "Bash(git show:*)", "Bash(ls:*)", "Bash(cat:*)"}
	var derived []string
	for name := range a.ExtraMCP {
		derived = append(derived, "mcp__"+name+"__*")
	}
	seen := map[string]bool{}
	for _, g := range a.Gate {
		if f := strings.Fields(g); len(f) > 0 && !seen[f[0]] {
			seen[f[0]] = true
			derived = append(derived, "Bash("+f[0]+":*)")
		}
	}
	sort.Strings(derived)
	return append(append(base, derived...), extra...)
}

type Adapter interface {
	// Command builds the process. The caller sets the process group, output and timeout.
	Command(ctx context.Context, a Attempt) (*exec.Cmd, error)
}

func New(id string, p config.Provider, prof config.Profile) (Adapter, error) {
	switch p.Kind {
	case "claude":
		return claude{id: id, p: p, prof: prof}, nil
	case "codex":
		return nil, fmt.Errorf("provider %s: codex adapter lands in phase 4", id)
	case "mock":
		return mock{p: p, prof: prof}, nil
	}
	return nil, fmt.Errorf("provider %s: unknown kind %q", id, p.Kind)
}

// SwarmEnv is what every worker gets, whatever the CLI: who it is and where the board is.
func SwarmEnv(a Attempt) []string {
	return []string{
		"SWARM_TASK_ID=" + strconv.FormatInt(a.TaskID, 10),
		"SWARM_WORKTREE=" + a.Worktree,
		"SWARM_SOCKET=" + a.Socket,
		"SWARM_TOKEN=" + a.Token,
		"SWARM_BIN=" + a.SwarmBin,
	}
}

// ResolveEnv turns secret:/env: references into values. Plain values pass through.
func ResolveEnv(ctx context.Context, env map[string]string, prof config.Profile) ([]string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		v := env[k]
		switch {
		case strings.HasPrefix(v, "env:"):
			name := strings.TrimPrefix(v, "env:")
			val, ok := os.LookupEnv(name)
			if !ok {
				return nil, fmt.Errorf("env %s: %s is not set", k, name)
			}
			v = val
		case strings.HasPrefix(v, "secret:"):
			if len(prof.SecretCmd) == 0 {
				return nil, fmt.Errorf("env %s: %s needs profile.secret_cmd", k, v)
			}
			argv := append(append([]string{}, prof.SecretCmd...), strings.TrimPrefix(v, "secret:"))
			var o, e bytes.Buffer
			c := exec.CommandContext(ctx, argv[0], argv[1:]...)
			c.Stdout, c.Stderr = &o, &e
			if err := c.Run(); err != nil {
				// the value is never printed; only that it could not be read
				return nil, fmt.Errorf("env %s: secret_cmd failed: %v", k, err)
			}
			v = strings.TrimSpace(o.String())
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

type claude struct {
	id   string
	p    config.Provider
	prof config.Profile
}

// Flags per PRD §8, checked against claude 2.1.325 --help. Subagents are off by flag, not by
// prose: Agent is the current name, Task the old one, Workflow fans out too.
func (c claude) Command(ctx context.Context, a Attempt) (*exec.Cmd, error) {
	mcpPath, err := WriteMCPConfig(ctx, a, c.prof)
	if err != nil {
		return nil, err
	}
	args := []string{"-p", a.Prompt, "--output-format", "stream-json", "--verbose",
		"--mcp-config", mcpPath, "--strict-mcp-config",
		"--disallowedTools", "Agent,Task,Workflow",
		"--allowedTools", strings.Join(AllowedTools(a, c.p.AllowedTools), ","),
		"--permission-mode", "acceptEdits"}
	if c.p.SettingSources != "" {
		args = append(args, "--setting-sources", c.p.SettingSources)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = a.Worktree
	env, err := ResolveEnv(ctx, c.p.Env, c.prof)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(append(CleanEnv(), env...), SwarmEnv(a)...)
	if c.p.ConfigDir != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+config.Expand(c.p.ConfigDir))
	}
	return cmd, nil
}

// WriteMCPConfig writes the Claude-format MCP config: the board plus the profile's extra
// servers for this role, with their env references resolved.
func WriteMCPConfig(ctx context.Context, a Attempt, prof config.Profile) (string, error) {
	servers := map[string]any{"board": map[string]any{
		"command": a.SwarmBin, "args": []string{"mcp", "--socket", a.Socket, "--token", a.Token},
	}}
	for name, m := range a.ExtraMCP {
		env, err := ResolveEnv(ctx, m.Env, prof)
		if err != nil {
			return "", fmt.Errorf("mcp %s: %w", name, err)
		}
		envMap := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			envMap[k] = v
		}
		args := m.Args
		if args == nil {
			args = []string{}
		}
		servers[name] = map[string]any{"command": config.Expand(m.Command), "args": args, "env": envMap}
	}
	b, _ := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", " ")
	path := filepath.Join(a.Dir, "mcp.json")
	return path, os.WriteFile(path, b, 0o600)
}

// inherited is what a worker must never pick up from the daemon's environment: it would
// silently run on another account (CLAUDE_CONFIG_DIR) or switch a subscription worker to
// API billing (ANTHROPIC_API_KEY). A provider that wants one sets it in its own env.
var inherited = []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "SWARM_PROFILE"}

func CleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(inherited, k) {
			out = append(out, kv)
		}
	}
	return out
}

// AuthFailure: the CLI could not log in. Not the task's fault and not worth a retry on the
// same provider: the provider is taken out until a person logs it in again.
var authPatterns = []string{"Not logged in", "Invalid API key", "OAuth token has expired", "Please run /login"}

func AuthFailure(logTail string) bool {
	for _, p := range authPatterns {
		if strings.Contains(logTail, p) {
			return true
		}
	}
	return false
}

type mock struct {
	p    config.Provider
	prof config.Profile
}

// mock runs a scripted worker: the same protocol (board over the socket, report done),
// no model, no quota. Tests and dry runs use it.
func (m mock) Command(ctx context.Context, a Attempt) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, m.p.Command[0], m.p.Command[1:]...)
	cmd.Dir = a.Worktree
	env, err := ResolveEnv(ctx, m.p.Env, m.prof)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(append(CleanEnv(), env...), SwarmEnv(a)...)
	mcpPath, err := WriteMCPConfig(ctx, a, m.prof)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(cmd.Env, "SWARM_PROMPT_FILE="+filepath.Join(a.Dir, "prompt.md"), "SWARM_MCP_CONFIG="+mcpPath)
	return cmd, nil
}

// Usage of one attempt, from the CLI's final event. Zero values mean the CLI did not say;
// they are stored as NULL, never guessed.
type Usage struct {
	TokensIn, TokensOut int64
	CostUSD             float64
	Known               bool
}

// ParseUsage reads Claude's stream-json: the last {"type":"result"} line carries the
// attempt's total cost and token counts (input counts cache reads and writes too).
func ParseUsage(log string) Usage {
	var u Usage
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, `"type":"result"`) {
			continue
		}
		var e struct {
			Type  string  `json:"type"`
			Cost  float64 `json:"total_cost_usd"`
			Usage struct {
				In          int64 `json:"input_tokens"`
				Out         int64 `json:"output_tokens"`
				CacheRead   int64 `json:"cache_read_input_tokens"`
				CacheCreate int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Type != "result" {
			continue
		}
		u = Usage{TokensIn: e.Usage.In + e.Usage.CacheRead + e.Usage.CacheCreate, TokensOut: e.Usage.Out, CostUSD: e.Cost, Known: true}
	}
	return u
}
