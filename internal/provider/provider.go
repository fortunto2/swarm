// Package provider turns one attempt into one process of an official CLI. Nothing here
// wraps a subscription's OAuth into an API: each provider runs its own binary.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/fortunto2/swarm/internal/config"
	"github.com/fortunto2/swarm/internal/envx"
)

// Attempt is everything a worker process needs. Dir holds the prompt, the MCP config and
// the log; it lives in the profile's state, never in the worktree (an agent's `git add -A`
// would commit it).
type Attempt struct {
	TaskID   int64
	Worktree string
	Dir      string
	Prompt   string
	Model    string
	Socket   string
	Token    string
	SwarmBin string
	ExtraMCP map[string]config.MCPServer
	Gate     []string       // the repo's gate commands; their programs are allowed to run
	Slot     int            // the worker's slot: its port range and anything else that must not collide
	Deny     []string       // absolute paths the worker may neither read nor edit (state, accounts, keys)
	Sandbox  config.Sandbox // OS confinement of the worker's Bash and its children
	Strict   bool           // safety = strict: the worker is treated as untrusted
}

// WorkerSettings: the --settings JSON of a worker.
//
// Permission deny rules cover Claude's own tools: credential paths may not be read or
// edited (the CLI reads anywhere otherwise: measured), and inside the worktree the files
// that configure an agent or git (.claude/, .mcp.json, CLAUDE.md, AGENTS.md, .git) may not
// be edited, so a worker cannot widen its own permissions for its next attempt.
//
// The sandbox covers Bash and every process it starts, which deny rules do not: `go test`
// runs the worker's code. Reads of the same paths are denied, writes go to the worktree
// and the build caches only, the network to the listed domains only, and a sandbox that
// cannot start fails the run instead of running unconfined.
func WorkerSettings(a Attempt) string {
	var rules []string
	add := func(p string, tools ...string) {
		p = strings.TrimSuffix(p, "/")
		for _, tool := range tools {
			rules = append(rules, tool+"(/"+p+"/**)", tool+"(/"+p+")")
		}
	}
	for _, p := range a.Deny {
		add(p, "Read", "Edit", "Write", "Glob", "Grep")
	}
	if a.Strict && a.Worktree != "" {
		for _, f := range []string{".claude", ".mcp.json", "CLAUDE.md", "AGENTS.md", ".git"} {
			add(filepath.Join(a.Worktree, f), "Edit", "Write")
		}
	}
	settings := map[string]any{"permissions": map[string]any{"deny": rules}}
	if a.Sandbox.On(a.Strict) {
		domains := a.Sandbox.AllowedDomains
		if domains == nil {
			domains = []string{}
		}
		settings["sandbox"] = map[string]any{
			"enabled":                  true,
			"autoAllowBashIfSandboxed": true,
			"allowUnsandboxedCommands": false,
			"failIfUnavailable":        true,
			"filesystem": map[string]any{
				"allowWrite": a.Sandbox.AllowWrite,
				"denyRead":   a.Deny,
			},
			// the board's socket is the one local door a worker needs (swarm gate, swarm board)
			"network": map[string]any{"allowedDomains": domains, "strictAllowlist": true,
				"allowUnixSockets": []string{a.Socket}},
		}
	}
	b, _ := json.Marshal(settings)
	return string(b)
}

// AllowedTools: with no sandbox and nobody to answer a prompt, a worker may run only what
// its job needs. File tools are absent: acceptEdits already allows editing inside the
// working directory; reading is allowed anywhere by the CLI, which is why credential paths
// get explicit deny rules (DenySettings). git diff/log/show are out: --output writes
// anywhere. No push.
func AllowedTools(a Attempt, extra []string) []string {
	base := []string{"mcp__board__*",
		"Bash(swarm gate:*)", "Bash(swarm board:*)",
		"Bash(git add:*)", "Bash(git commit:*)", "Bash(git status:*)", "Bash(git rev-parse:*)"}
	var derived []string
	for name, m := range a.ExtraMCP { // the tools listed for the server; all of them if none listed
		if len(m.Tools) == 0 {
			derived = append(derived, "mcp__"+name+"__*")
		}
		for _, tool := range m.Tools {
			derived = append(derived, "mcp__"+name+"__"+tool)
		}
	}
	seen := map[string]bool{}
	for _, g := range a.Gate {
		if prog := program(g); prog != "" && !seen[prog] {
			seen[prog] = true
			derived = append(derived, "Bash("+prog+":*)")
		}
	}
	slices.Sort(derived)
	return append(append(base, derived...), extra...)
}

// program is the tool a gate command runs, for a permission rule: env assignments are
// skipped, and a shell, cd or env yields nothing (allowing `sh:*` would allow anything;
// the worker has `swarm gate` for the exact check instead).
func program(cmdline string) string {
	for _, f := range strings.Fields(cmdline) {
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "=") {
			continue
		}
		switch f {
		// shells and wrappers would allow anything; git and swarm have their own narrow
		// rules above (a gate command `git diff --exit-code` must not become git:*)
		case "sh", "bash", "zsh", "cd", "env", "exec", "sudo", "eval", "xargs", "git", "swarm", "curl", "wget":
			return ""
		}
		return f
	}
	return ""
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
		"SWARM_SLOT=" + strconv.Itoa(a.Slot),
		"PORT=" + strconv.Itoa(20000+a.Slot*10), // 10 ports per slot: PORT..PORT+9
		"DISABLE_AUTOUPDATER=1",                 // a CLI that updates itself mid-run changes its flags and JSON
		// swarm on PATH, so `swarm gate` matches its permission rule literally
		"PATH=" + filepath.Dir(a.SwarmBin) + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
}

// ResolveEnv turns secret:/env: references into values. Plain values pass through.
func ResolveEnv(ctx context.Context, env map[string]string, prof config.Profile) ([]string, error) {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
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
	args = append(args, "--settings", WorkerSettings(a))
	// Settings come from the operator's user level only by default: their hooks and skills
	// help; the worked-on repo's .claude/ settings could widen the worker (permissions,
	// hooks run outside the sandbox) and are not trusted unless the provider says so.
	sources := c.p.SettingSources
	if sources == "" && a.Strict {
		sources = "user"
	}
	if sources != "" {
		args = append(args, "--setting-sources", sources)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	cmd, err := baseCmd(ctx, "claude", args, a, c.p, c.prof)
	if err != nil {
		return nil, err
	}
	if c.p.ConfigDir != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+config.Expand(c.p.ConfigDir))
	}
	return cmd, nil
}

// WriteMCPConfig writes the Claude-format MCP config: the board plus the profile's extra
// servers for this role, with their env references resolved.
func WriteMCPConfig(ctx context.Context, a Attempt, prof config.Profile) (string, error) {
	// the token travels in the server's environment, not its argv: argv is in `ps` for anyone
	servers := map[string]any{"board": map[string]any{
		"command": a.SwarmBin, "args": []string{"mcp", "--socket", a.Socket},
		"env": map[string]string{"SWARM_TOKEN": a.Token},
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

// CleanEnv: the worker's base environment, without accounts, billing routes or credentials
// (see envx).
func CleanEnv() []string { return envx.Clean() }

// baseCmd: the process every adapter starts from. In the worktree, with a clean
// environment, the provider's resolved env and the swarm identity.
func baseCmd(ctx context.Context, name string, args []string, a Attempt, p config.Provider, prof config.Profile) (*exec.Cmd, error) {
	env, err := ResolveEnv(ctx, p.Env, prof)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = a.Worktree
	base := envx.Clean() // trusted: drop accounts and billing routes only
	if a.Strict {
		base = envx.Minimal() // strict: an allowlist, unusual credential names included
	}
	cmd.Env = append(append(base, env...), SwarmEnv(a)...)
	return cmd, nil
}

type mock struct {
	p    config.Provider
	prof config.Profile
}

// mock runs a scripted worker: the same protocol (board over the socket, report done),
// no model, no quota. Tests and dry runs use it.
func (m mock) Command(ctx context.Context, a Attempt) (*exec.Cmd, error) {
	cmd, err := baseCmd(ctx, m.p.Command[0], m.p.Command[1:], a, m.p, m.prof)
	if err != nil {
		return nil, err
	}
	mcpPath, err := WriteMCPConfig(ctx, a, m.prof)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(cmd.Env, "SWARM_PROMPT_FILE="+filepath.Join(a.Dir, "prompt.md"), "SWARM_MCP_CONFIG="+mcpPath)
	return cmd, nil
}

// Result of one attempt, from the CLI's final event. Zero usage means the CLI did not say;
// it is stored as NULL, never guessed.
type Result struct {
	TokensIn, TokensOut int64
	CostUSD             float64
	Known               bool   // a final result event was found
	AuthFailed          bool   // the CLI could not log in: not the task's fault, the provider is out
	Refused             string // the model refused the task under its usage policy: retrying will not help
}

// refusalPatterns: the CLI's wording when a session is stopped by the model's safeguards.
// Measured 2026-11-15: "API Error: Sonnet 5.5's safeguards flagged this session (…aup)".
var refusalPatterns = []string{"safeguards flagged", "Usage Policy", "legal/aup"}

// authPatterns are matched against the final result's text only, never the whole log: a
// worker that greps a file mentioning /login must not take its provider down.
var authPatterns = []string{"Not logged in", "Invalid API key", "OAuth token has expired", "Please run /login"}

func authText(s string) bool {
	return slices.ContainsFunc(authPatterns, func(p string) bool { return strings.Contains(s, p) })
}

// ParseResult reads Claude's stream-json: the last {"type":"result"} line carries the
// attempt's cost, token counts (input includes cache reads and writes) and, on failure, the
// reason. With no result event, only the last line of output is checked for a login error.
func ParseResult(log string) Result {
	var r Result
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	for _, line := range lines {
		if !strings.Contains(line, `"type":"result"`) {
			continue
		}
		var e struct {
			Type    string  `json:"type"`
			IsError bool    `json:"is_error"`
			Text    string  `json:"result"`
			Cost    float64 `json:"total_cost_usd"`
			Usage   struct {
				In          int64 `json:"input_tokens"`
				Out         int64 `json:"output_tokens"`
				CacheRead   int64 `json:"cache_read_input_tokens"`
				CacheCreate int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Type != "result" {
			continue
		}
		r = Result{TokensIn: e.Usage.In + e.Usage.CacheRead + e.Usage.CacheCreate, TokensOut: e.Usage.Out,
			CostUSD: e.Cost, Known: true, AuthFailed: e.IsError && authText(e.Text)}
		if e.IsError && slices.ContainsFunc(refusalPatterns, func(p string) bool { return strings.Contains(e.Text, p) }) {
			r.Refused = e.Text
		}
	}
	if !r.Known && len(lines) > 0 {
		r.AuthFailed = authText(lines[len(lines)-1])
	}
	return r
}
