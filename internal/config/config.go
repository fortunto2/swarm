// Package config loads a profile's swarm.toml. A profile is one world (solo, ep): its
// providers, repos, nodes and policies. The core never hard-codes any of them.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

type Profile struct {
	Name         string   `toml:"name"`
	StateDir     string   `toml:"state_dir"`     // relative to the profile dir unless absolute
	WorktreeRoot string   `toml:"worktree_root"` // outside every repo, never <repo>/.swarm/wt
	Notify       string   `toml:"notify"`        // command that takes the message as last arg
	Harness      string   `toml:"harness"`       // harness-core profile dir, for runs.jsonl
	SecretCmd    []string `toml:"secret_cmd"`    // resolves secret:<name>: argv + name, value on stdout
	ProjectRules string   `toml:"project_rules"` // file appended to every worker prompt (prompts/project.md)
}

type Repo struct {
	Path        string   `toml:"path"`
	MainBranch  string   `toml:"main_branch"`
	Gate        []string `toml:"gate"`
	GateTimeout Duration `toml:"gate_timeout"`
	Merge       string   `toml:"merge"` // "ff": ff-only into main; "pr": leave a branch for a human PR
}

type Limits struct {
	MaxWorkers    int      `toml:"max_workers"`
	TaskWallClock Duration `toml:"task_wall_clock"`
	MaxAttempts   int      `toml:"max_attempts"`
}

type Provider struct {
	Kind      string            `toml:"kind"`   // claude | codex | mock
	Family    string            `toml:"family"` // reviewer must come from another family
	ConfigDir string            `toml:"config_dir"`
	Home      string            `toml:"home"`
	Env       map[string]string `toml:"env"` // secret-looking keys must be refs: secret:<name> or env:<VAR>
	Slots     int               `toml:"slots"`
	Tiers     []string          `toml:"tiers"`
	Models    map[string]string `toml:"models"`
	BudgetUSD float64           `toml:"budget_usd"`
	Command   []string          `toml:"command"` // kind=mock only: argv of the fake worker
	// kind=claude: which settings load (user,project,local). Empty = the CLI's default, all
	// of them: the operator's hooks and skills help workers too.
	SettingSources string `toml:"setting_sources"`
	// kind=claude: tools allowed without a prompt, on top of what swarm derives (board and
	// extra MCP tools, git on the own branch, the programs of the repo's gate).
	AllowedTools []string `toml:"allowed_tools"`
}

type Node struct {
	Host       string   `toml:"host"` // "local" or "ssh:<alias>"
	MaxWorkers int      `toml:"max_workers"`
	Accounts   []string `toml:"accounts"`
	Roles      []string `toml:"roles"`
}

// MCPServer: an extra read-only server given to agents next to the board, e.g. a code
// and session index so a worker can find where a name is defined or what an earlier
// attempt tried. Roles: "worker", "lead".
type MCPServer struct {
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
	Roles   []string          `toml:"roles"`
}

// Hooks run after events, best effort: a failing hook is logged, never undoes a merge.
// Placeholders: {repo} (name), {path} (repo path), {task}, {commit}.
type Hooks struct {
	AfterMerge []string `toml:"after_merge"`
	Timeout    Duration `toml:"timeout"`
}

type Config struct {
	Dir       string               `toml:"-"` // the profile directory this file was read from
	Profile   Profile              `toml:"profile"`
	Repos     map[string]Repo      `toml:"repos"`
	Limits    Limits               `toml:"limits"`
	Providers map[string]Provider  `toml:"providers"`
	Nodes     map[string]Node      `toml:"nodes"`
	MCP       map[string]MCPServer `toml:"mcp"`
	Hooks     Hooks                `toml:"hooks"`
}

var (
	kinds      = []string{"claude", "codex", "mock"}
	tiers      = []string{"S", "A", "B"}
	merges     = []string{"ff", "pr"}
	roles      = []string{"supervisor", "tree", "tick", "script", "event"}
	secretKey  = regexp.MustCompile(`(?i)(TOKEN|KEY|SECRET|PASSWORD)`)
	refPattern = regexp.MustCompile(`^(secret|env):[A-Za-z0-9_./-]+$`)
)

// ResolveProfileDir: --profile flag, else SWARM_PROFILE. A bare name is not guessed.
func ResolveProfileDir(flag string) (string, error) {
	p := flag
	if p == "" {
		p = os.Getenv("SWARM_PROFILE")
	}
	if p == "" {
		return "", errors.New("no profile: pass --profile <dir> or set SWARM_PROFILE")
	}
	return filepath.Abs(Expand(p))
}

func Expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	return p
}

func Load(profileDir string) (*Config, error) {
	path := filepath.Join(profileDir, "swarm.toml")
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	c.Dir = profileDir
	c.defaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) defaults() {
	if c.Profile.StateDir == "" {
		c.Profile.StateDir = "state"
	}
	if c.Limits.MaxWorkers == 0 {
		c.Limits.MaxWorkers = 4
	}
	if c.Limits.TaskWallClock.Duration == 0 {
		c.Limits.TaskWallClock.Duration = 45 * time.Minute
	}
	if c.Limits.MaxAttempts == 0 {
		c.Limits.MaxAttempts = 4
	}
	if c.Hooks.Timeout.Duration == 0 {
		c.Hooks.Timeout.Duration = 2 * time.Minute
	}
	for n, r := range c.Repos {
		if r.MainBranch == "" {
			r.MainBranch = "main"
		}
		if r.GateTimeout.Duration == 0 {
			r.GateTimeout.Duration = 15 * time.Minute
		}
		c.Repos[n] = r
	}
}

// Validate returns every problem at once: fixing a config one error per run is how
// people stop reading the errors.
func (c *Config) Validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }

	if c.Profile.Name == "" {
		bad("profile.name is required")
	}
	if c.Profile.WorktreeRoot == "" {
		bad("profile.worktree_root is required (outside every repo)")
	}
	if len(c.Repos) == 0 {
		bad("at least one [repos.<name>] is required")
	}
	for _, n := range sortedKeys(c.Repos) {
		r := c.Repos[n]
		if r.Path == "" {
			bad("repos.%s.path is required", n)
		}
		if len(r.Gate) == 0 {
			bad("repos.%s.gate is empty: nothing would decide a task is done", n)
		}
		if !slices.Contains(merges, r.Merge) {
			bad("repos.%s.merge = %q, want one of %v", n, r.Merge, merges)
		}
		wt, _ := filepath.Abs(Expand(c.Profile.WorktreeRoot))
		rp, _ := filepath.Abs(Expand(r.Path))
		if r.Path != "" && c.Profile.WorktreeRoot != "" && (wt == rp || strings.HasPrefix(wt, rp+string(filepath.Separator))) {
			bad("profile.worktree_root is inside repos.%s: `git add -A` would sweep worktrees into a commit", n)
		}
	}
	if len(c.Providers) == 0 {
		bad("at least one [providers.<id>] is required")
	}
	for _, id := range sortedKeys(c.Providers) {
		p := c.Providers[id]
		if !slices.Contains(kinds, p.Kind) {
			bad("providers.%s.kind = %q, want one of %v", id, p.Kind, kinds)
		}
		if p.Family == "" {
			bad("providers.%s.family is required (cross-family review)", id)
		}
		if p.Slots < 1 {
			bad("providers.%s.slots must be >= 1", id)
		}
		if p.Kind == "mock" && len(p.Command) == 0 {
			bad("providers.%s: kind mock needs command", id)
		}
		for _, t := range p.Tiers {
			if !slices.Contains(tiers, t) {
				bad("providers.%s.tiers: %q is not one of %v", id, t, tiers)
			}
		}
		for _, k := range sortedKeys(p.Env) {
			if secretKey.MatchString(k) && !refPattern.MatchString(p.Env[k]) {
				bad("providers.%s.env.%s holds a value; use secret:<name> or env:<VAR>", id, k)
			}
		}
	}
	for _, n := range sortedKeys(c.Nodes) {
		nd := c.Nodes[n]
		if nd.Host != "local" && !strings.HasPrefix(nd.Host, "ssh:") {
			bad("nodes.%s.host = %q, want \"local\" or \"ssh:<alias>\"", n, nd.Host)
		}
		for _, a := range nd.Accounts {
			if _, ok := c.Providers[a]; !ok {
				bad("nodes.%s.accounts: %q is not a provider", n, a)
			}
		}
		for _, r := range nd.Roles {
			if !slices.Contains(roles, r) {
				bad("nodes.%s.roles: %q is not one of %v", n, r, roles)
			}
		}
	}
	for _, n := range sortedKeys(c.MCP) {
		m := c.MCP[n]
		if n == "board" {
			bad("mcp.board is reserved for the swarm board")
		}
		if m.Command == "" {
			bad("mcp.%s.command is required", n)
		}
		for _, r := range m.Roles {
			if r != "worker" && r != "lead" {
				bad("mcp.%s.roles: %q is not worker or lead", n, r)
			}
		}
		for _, k := range sortedKeys(m.Env) {
			if secretKey.MatchString(k) && !refPattern.MatchString(m.Env[k]) {
				bad("mcp.%s.env.%s holds a value; use secret:<name> or env:<VAR>", n, k)
			}
		}
	}
	return errors.Join(errs...)
}

// MCPFor: the extra servers a role gets; no roles listed means every role.
func (c *Config) MCPFor(role string) map[string]MCPServer {
	out := map[string]MCPServer{}
	for n, m := range c.MCP {
		if len(m.Roles) == 0 || slices.Contains(m.Roles, role) {
			out[n] = m
		}
	}
	return out
}

func (c *Config) StatePath() string {
	s := Expand(c.Profile.StateDir)
	if filepath.IsAbs(s) {
		return s
	}
	return filepath.Join(c.Dir, s)
}

func (c *Config) DBPath() string { return filepath.Join(c.StatePath(), "swarm.db") }

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
