package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/fortunto2/swarm/internal/config"
)

// codex: OpenAI's Codex CLI, `codex exec`, one account per CODEX_HOME (provider home, or
// config_dir). Checked against codex-cli 0.147 --help and its config keys.
//
// Confinement follows the profile's sandbox setting, as for a Claude worker. Off (trusted,
// the default): commands run unconfined (danger-full-access), as a trusted Claude worker's
// Bash does; Codex has no tool allowlist, so the prompt and the gate are the guard. On:
// Codex's workspace-write sandbox, writable the worktree, the repo's shared .git (a worktree
// commits there) and the profile's sandbox.allow_write, with the network on (Codex cannot
// allowlist domains, and the board's unix socket needs it). Strict is refused for now: a
// writable shared .git would let an untrusted worker plant config the daemon's own git reads.
// Approvals are off: nobody is there to answer one.
//
// The board is an MCP server given with -c overrides; its token reaches it by name
// (env_vars), and the prompt goes on stdin, so neither is in argv for `ps` to show. The
// model's shell gets the worker environment with SWARM_TOKEN (swarm gate and swarm board
// need it, and Codex drops any *TOKEN* by default) and without anything else secret-looking
// or meant for an MCP server alone.
type codex struct {
	id   string
	p    config.Provider
	prof config.Profile
}

func (c codex) Command(ctx context.Context, a Attempt) (*exec.Cmd, error) {
	if a.Strict {
		return nil, fmt.Errorf("provider %s: codex workers are not run under safety = \"strict\" yet "+
			"(they commit into the repo's shared .git, which an untrusted worker could seed with config)", c.id)
	}
	args := []string{"exec", "--json", "-C", a.Worktree, "-c", `approval_policy="never"`}
	if a.Sandbox.On(false) {
		args = append(args, "--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true")
		roots := append([]string{}, a.Sandbox.AllowWrite...)
		if common := gitCommonDir(ctx, a.Worktree); common != "" {
			roots = append(roots, common)
		}
		for _, r := range roots {
			args = append(args, "--add-dir", config.Expand(r))
		}
	} else {
		args = append(args, "--sandbox", "danger-full-access")
	}
	args = append(args, mcpFlags("board", a.SwarmBin, []string{"mcp", "--socket", a.Socket}, []string{"SWARM_TOKEN"})...)
	var extraEnv []string
	serverOnly := map[string]string{} // env names forwarded to one MCP server: not the shell's, and one server each
	for _, name := range slices.Sorted(maps.Keys(a.ExtraMCP)) {
		m := a.ExtraMCP[name]
		command, sargs, env := config.Expand(m.Command), m.Args, m.Env
		if wc, wa, rest, ok := wrapSecrets(m, c.prof); ok {
			// secrets reach the server through the keystore's exec, by name: never in this env
			command, sargs, env = wc, wa, rest
		}
		resolved, err := ResolveEnv(ctx, env, c.prof)
		if err != nil {
			return nil, fmt.Errorf("mcp %s: %w", name, err)
		}
		var names []string
		for _, kv := range resolved {
			k, _, _ := strings.Cut(kv, "=")
			if prev, ok := serverOnly[k]; ok && prev != name {
				return nil, fmt.Errorf("mcp %s and %s both set %s: a codex worker forwards env by name, rename one", prev, name, k)
			}
			serverOnly[k] = name
			names = append(names, k)
		}
		extraEnv = append(extraEnv, resolved...)
		args = append(args, mcpFlags(name, command, sargs, names)...)
	}
	if a.Model != "" {
		args = append(args, "-m", a.Model)
	}
	cmd, err := baseCmd(ctx, "codex", args, a, c.p, c.prof)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	home := c.p.Home
	if home == "" {
		home = c.p.ConfigDir
	}
	if home != "" {
		cmd.Env = append(cmd.Env, "CODEX_HOME="+config.Expand(home))
	}
	cmd.Args = append(cmd.Args, shellEnvPolicy(cmd.Env, serverOnly)...)
	cmd.Args = append(cmd.Args, "-") // the prompt, from stdin
	cmd.Stdin = strings.NewReader(a.Prompt)
	return cmd, nil
}

// shellEnvPolicy: what the model's shell inherits. Codex's default excludes (any name with
// KEY, SECRET or TOKEN) would take SWARM_TOKEN too, so they are replaced by an explicit list
// of the names actually present: the secret-looking ones but SWARM_TOKEN, and every name
// meant for an MCP server alone. Names only: values never reach argv.
func shellEnvPolicy(env []string, serverOnly map[string]string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		_, forServer := serverOnly[k]
		// GIT_CONFIG_COUNT/KEY_n/VALUE_n are git config, not secrets: dropping KEY_n alone (as Codex's
		// own default would) leaves git in the shell failing on every call (seen in the e2e's env)
		secret := !strings.HasPrefix(u, "GIT_CONFIG_") && (strings.Contains(u, "KEY") || strings.Contains(u, "SECRET") ||
			strings.Contains(u, "TOKEN") || strings.Contains(u, "PASSWORD") || strings.Contains(u, "CREDENTIAL"))
		if k != "SWARM_TOKEN" && (secret || forServer) && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	if out == nil {
		out = []string{}
	}
	b, _ := json.Marshal(out)
	return []string{"-c", "shell_environment_policy.ignore_default_excludes=true",
		"-c", "shell_environment_policy.exclude=" + string(b)}
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// mcpFlags: one MCP server as codex -c overrides. Values are TOML; a JSON string or array of
// strings is valid TOML, so json.Marshal quotes them.
func mcpFlags(name, command string, args, envNames []string) []string {
	q := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if args == nil {
		args = []string{}
	}
	key := name
	if !bareKey.MatchString(name) { // "my.index" would be a nested key
		key = q(name)
	}
	out := []string{"-c", fmt.Sprintf("mcp_servers.%s.command=%s", key, q(command)),
		"-c", fmt.Sprintf("mcp_servers.%s.args=%s", key, q(args))}
	if len(envNames) > 0 {
		out = append(out, "-c", fmt.Sprintf("mcp_servers.%s.env_vars=%s", key, q(envNames)))
	}
	return out
}

// gitCommonDir: the repo's shared .git for a worktree, absolute; "" when it cannot be read.
func gitCommonDir(ctx context.Context, wt string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", wt, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		return ""
	}
	return dir
}
