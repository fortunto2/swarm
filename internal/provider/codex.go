package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fortunto2/swarm/internal/config"
)

// codex: OpenAI's Codex CLI, `codex exec`, one account per CODEX_HOME (provider home, or
// config_dir). Checked against codex-cli 0.147 --help and its config keys.
//
// Where Claude workers get a tool allowlist, a Codex worker gets Codex's own sandbox:
// workspace-write, so commands write only inside the worktree and the repo's shared .git
// (added with --add-dir: a worktree commits into the main repo's object store). Trusted
// mode lets it reach the network (installs, the board socket for `swarm gate`); strict mode
// does not. Approvals are off: nobody is there to answer one.
//
// The board is an MCP server given with -c overrides. Its token goes by name (env_vars), so
// it is in the worker's environment, never in argv, which anyone can read in `ps`; the
// prompt goes on stdin for the same reason.
type codex struct {
	id   string
	p    config.Provider
	prof config.Profile
}

func (c codex) Command(ctx context.Context, a Attempt) (*exec.Cmd, error) {
	args := []string{"exec", "--json", "--sandbox", "workspace-write", "-C", a.Worktree,
		"-c", `approval_policy="never"`}
	if common := gitCommonDir(ctx, a.Worktree); common != "" {
		args = append(args, "--add-dir", common)
	}
	if !a.Strict {
		args = append(args, "-c", "sandbox_workspace_write.network_access=true")
	}
	args = append(args, mcpFlags("board", a.SwarmBin, []string{"mcp", "--socket", a.Socket}, []string{"SWARM_TOKEN"})...)
	var extraEnv []string
	owner := map[string]string{} // an env name forwarded to one server only: two servers, one name, two values would clash
	for _, name := range slices.Sorted(maps.Keys(a.ExtraMCP)) {
		m := a.ExtraMCP[name]
		env, err := ResolveEnv(ctx, m.Env, c.prof)
		if err != nil {
			return nil, fmt.Errorf("mcp %s: %w", name, err)
		}
		var names []string
		for _, kv := range env {
			k, _, _ := strings.Cut(kv, "=")
			if prev, ok := owner[k]; ok && prev != name {
				return nil, fmt.Errorf("mcp %s and %s both set %s: a codex worker forwards env by name, rename one", prev, name, k)
			}
			owner[k] = name
			names = append(names, k)
		}
		extraEnv = append(extraEnv, env...)
		args = append(args, mcpFlags(name, config.Expand(m.Command), m.Args, names)...)
	}
	if a.Model != "" {
		args = append(args, "-m", a.Model)
	}
	args = append(args, "-") // the prompt, from stdin
	cmd, err := baseCmd(ctx, "codex", args, a, c.p, c.prof)
	if err != nil {
		return nil, err
	}
	cmd.Stdin = strings.NewReader(a.Prompt)
	cmd.Env = append(cmd.Env, extraEnv...)
	home := c.p.Home
	if home == "" {
		home = c.p.ConfigDir
	}
	if home != "" {
		cmd.Env = append(cmd.Env, "CODEX_HOME="+config.Expand(home))
	}
	return cmd, nil
}

// mcpFlags: one MCP server as codex -c overrides. Values are TOML; a JSON string or array of
// strings is valid TOML, so json.Marshal quotes them.
func mcpFlags(name, command string, args, envNames []string) []string {
	q := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if args == nil {
		args = []string{}
	}
	out := []string{"-c", fmt.Sprintf("mcp_servers.%s.command=%s", name, q(command)),
		"-c", fmt.Sprintf("mcp_servers.%s.args=%s", name, q(args))}
	if len(envNames) > 0 {
		out = append(out, "-c", fmt.Sprintf("mcp_servers.%s.env_vars=%s", name, q(envNames)))
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
