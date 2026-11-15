// Package config loads a profile's swarm.toml. A profile is one world (solo, ep): its
// providers, repos, nodes and policies. The core never hard-codes any of them.
package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/jsonschema-go/jsonschema"
)

type Duration struct{ time.Duration }

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

type Profile struct {
	Name         string   `toml:"name" json:"name,omitempty"`
	StateDir     string   `toml:"state_dir" json:"state_dir,omitempty"`         // relative to the profile dir unless absolute
	WorktreeRoot string   `toml:"worktree_root" json:"worktree_root,omitempty"` // outside every repo, never <repo>/.swarm/wt
	Notify       string   `toml:"notify" json:"notify,omitempty"`               // command that takes the message as last arg
	Harness      string   `toml:"harness" json:"harness,omitempty"`             // harness-core profile dir, for runs.jsonl
	SecretCmd    []string `toml:"secret_cmd" json:"secret_cmd,omitempty"`       // get: argv + name, value on stdout (e.g. solo secret get)
	SecretExec   []string `toml:"secret_exec" json:"secret_exec,omitempty"`     // exec: argv + NAME=ENV pairs + -- cmd (e.g. solo secret exec)
	Redact       []string `toml:"redact" json:"redact,omitempty"`               // filter for text swarm stores or forwards (e.g. secret_guard.py filter)
	ProjectRules string   `toml:"project_rules" json:"project_rules,omitempty"`
	DenyPaths    []string `toml:"deny_paths" json:"deny_paths,omitempty"` // extra paths workers may not read or edit
	// Safety: "trusted" (default) guards against mistakes at no cost to the work: the gate,
	// claims, test checks, credential paths denied. "strict" also treats the worker as
	// untrusted: OS sandbox, user-level settings only, env allowlist, explicit MCP tools,
	// protected paths and shrinking tests fail the gate, files required on every task.
	Safety string `toml:"safety" json:"safety,omitempty"` // file appended to every worker prompt (prompts/project.md)
}

type Repo struct {
	Path        string   `toml:"path" json:"path,omitempty"`
	MainBranch  string   `toml:"main_branch" json:"main_branch,omitempty"`
	Gate        []string `toml:"gate" json:"gate,omitempty"`
	GateTimeout Duration `toml:"gate_timeout" json:"gate_timeout,omitempty"`
	Merge       string   `toml:"merge" json:"merge,omitempty"`
	// merge = "branch": the gated commit (rebased on main) is published as BranchPrefix+"T<id>"
	// for a person or a review process to merge; main is never touched. Default "swarm/done/".
	BranchPrefix string `toml:"branch_prefix" json:"branch_prefix,omitempty"`
	GateReruns   *int   `toml:"gate_reruns" json:"gate_reruns,omitempty"` // reruns of a failed gate command (flaky tests); default 1 // "ff": ff-only into main; "pr": leave a branch for a human PR
}

type Limits struct {
	MaxWorkers    int      `toml:"max_workers" json:"max_workers,omitempty"`
	TaskWallClock Duration `toml:"task_wall_clock" json:"task_wall_clock,omitempty"`
	MaxAttempts   int      `toml:"max_attempts" json:"max_attempts,omitempty"`
	MaxGates      int      `toml:"max_gates" json:"max_gates,omitempty"` // gates running at once; default half of max_workers
}

type Provider struct {
	Kind      string            `toml:"kind" json:"kind,omitempty"`     // claude | codex | mock
	Family    string            `toml:"family" json:"family,omitempty"` // reviewer must come from another family
	ConfigDir string            `toml:"config_dir" json:"config_dir,omitempty"`
	Home      string            `toml:"home" json:"home,omitempty"`
	Env       map[string]string `toml:"env" json:"env,omitempty"` // secret-looking keys must be refs: secret:<name> or env:<VAR>
	Slots     int               `toml:"slots" json:"slots,omitempty"`
	Tiers     []string          `toml:"tiers" json:"tiers,omitempty"`
	Models    map[string]string `toml:"models" json:"models,omitempty"`
	// Registry fields. Roles: worker (default), reviewer (phase 4), lead (phase 2). Priority:
	// lower is tried first among providers that can take a tier. BudgetUSDDay: dollars per
	// UTC day, measured from the CLI's own cost reports; spent, the provider cools until
	// the next UTC midnight. 0 means no cap.
	Roles        []string `toml:"roles" json:"roles,omitempty"`
	Priority     int      `toml:"priority" json:"priority,omitempty"`
	BudgetUSDDay float64  `toml:"budget_usd_day" json:"budget_usd_day,omitempty"`
	// LimitsFile: a rate-limit reading in Claude Code's status-line shape
	// ({"limits": {"five_hour": {"used_percentage", "resets_at"}, "seven_day": {...}}}), written by
	// whatever watches this account. At PauseAbove percent (default 80) in any window the
	// provider cools until that window resets: the swarm leaves the rest for work that must run.
	LimitsFile string   `toml:"limits_file" json:"limits_file,omitempty"`
	PauseAbove float64  `toml:"pause_above" json:"pause_above,omitempty"`
	Command    []string `toml:"command" json:"command,omitempty"` // kind=mock only: argv of the fake worker
	// kind=claude: which settings load (user,project,local). Empty = the CLI's default, all
	// of them: the operator's hooks and skills help workers too.
	SettingSources string `toml:"setting_sources" json:"setting_sources,omitempty"`
	// kind=claude: tools allowed without a prompt, on top of what swarm derives (board and
	// extra MCP tools, git on the own branch, the programs of the repo's gate).
	AllowedTools []string `toml:"allowed_tools" json:"allowed_tools,omitempty"`
}

type Node struct {
	Host       string   `toml:"host" json:"host,omitempty"` // "local" or "ssh:<alias>"
	MaxWorkers int      `toml:"max_workers" json:"max_workers,omitempty"`
	Accounts   []string `toml:"accounts" json:"accounts,omitempty"`
	Roles      []string `toml:"roles" json:"roles,omitempty"`
}

// MCPServer: an extra read-only server given to agents next to the board, e.g. a code
// and session index so a worker can find where a name is defined or what an earlier
// attempt tried. Roles: "worker", "lead".
type MCPServer struct {
	Command string            `toml:"command" json:"command,omitempty"`
	Args    []string          `toml:"args" json:"args,omitempty"`
	Env     map[string]string `toml:"env" json:"env,omitempty"`
	Roles   []string          `toml:"roles" json:"roles,omitempty"`
	// Tools a worker may call on this server. Required for a server workers get: an MCP
	// server runs outside the worker's sandbox, so its reach is its tool list.
	Tools []string `toml:"tools" json:"tools,omitempty"`
}

// Hooks run after events, best effort: a failing hook is logged, never undoes a merge.
// Placeholders: {repo} (name), {path} (repo path), {task}, {commit}.
type Hooks struct {
	AfterMerge []string `toml:"after_merge" json:"after_merge,omitempty"`
	// OnStatus runs on every status change of a task, e.g. to mirror tasks on a board.
	// Placeholders: {task}, {title}, {status}, {worktree}, {repo}, {topic}, {commit}.
	// Best effort, bounded. after_merge has {repo} {path} {task} {commit} {topic} {title}.
	OnStatus []string `toml:"on_status" json:"on_status,omitempty"`
	Timeout  Duration `toml:"timeout" json:"timeout,omitempty"`
}

// Sandbox: how Claude Code confines a worker's Bash and every process it starts (Seatbelt
// on macOS, bubblewrap on Linux). On by default: without it, `go test` runs the worker's
// code as you, past every permission rule. Writes go only to the worktree plus AllowWrite
// (build caches); reads of credential paths are denied; the network is AllowedDomains only.
type Sandbox struct {
	Enabled        *bool    `toml:"enabled" json:"enabled,omitempty"`
	AllowWrite     []string `toml:"allow_write" json:"allow_write,omitempty"`
	AllowedDomains []string `toml:"allowed_domains" json:"allowed_domains,omitempty"`
}

// On: the sandbox runs if the profile says so, else if safety is strict.
func (s Sandbox) On(strict bool) bool {
	if s.Enabled != nil {
		return *s.Enabled
	}
	return strict
}

type Config struct {
	Dir       string               `toml:"-" json:"-"` // the profile directory this file was read from
	Profile   Profile              `toml:"profile" json:"profile,omitempty"`
	Repos     map[string]Repo      `toml:"repos" json:"repos,omitempty"`
	Limits    Limits               `toml:"limits" json:"limits,omitempty"`
	Providers map[string]Provider  `toml:"providers" json:"providers,omitempty"`
	Nodes     map[string]Node      `toml:"nodes" json:"nodes,omitempty"`
	MCP       map[string]MCPServer `toml:"mcp" json:"mcp,omitempty"`
	Hooks     Hooks                `toml:"hooks" json:"hooks,omitempty"`
	Sandbox   Sandbox              `toml:"sandbox" json:"sandbox,omitempty"`
}

var (
	kinds         = []string{"claude", "codex", "mock"}
	tiers         = []string{"S", "A", "B"}
	merges        = []string{"ff", "branch", "pr"}
	roles         = []string{"supervisor", "tree", "tick", "script", "event"}
	providerRoles = []string{"worker", "reviewer", "lead"}
	secretKey     = regexp.MustCompile(`(?i)(TOKEN|KEY|SECRET|PASSW|CREDENTIAL|AUTH|DSN|COOKIE|SESSION)`)
	refPattern    = regexp.MustCompile(`^(secret|env):[A-Za-z0-9_./-]+$`)
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
	c.resolvePaths()
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
	if c.Limits.MaxGates == 0 {
		c.Limits.MaxGates = max(1, c.Limits.MaxWorkers/2)
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
		if r.BranchPrefix == "" {
			r.BranchPrefix = "swarm/done/"
		}
		if r.GateReruns == nil {
			one := 1
			r.GateReruns = &one
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
	if c.Profile.Safety != "" && c.Profile.Safety != "trusted" && c.Profile.Safety != "strict" {
		bad("profile.safety = %q, want trusted or strict", c.Profile.Safety)
	}
	if c.Profile.WorktreeRoot == "" {
		bad("profile.worktree_root is required (outside every repo)")
	}
	if len(c.Repos) == 0 {
		bad("at least one [repos.<name>] is required")
	}
	for _, n := range slices.Sorted(maps.Keys(c.Repos)) {
		r := c.Repos[n]
		if r.Path == "" {
			bad("repos.%s.path is required", n)
		}
		if len(r.Gate) == 0 {
			bad("repos.%s.gate is empty: nothing would decide a task is done", n)
		}
		if strings.HasPrefix(r.MainBranch, "refs/") && r.Merge == "ff" {
			bad("repos.%s: main_branch %q is a full ref; merge = \"ff\" can only move a local branch (use merge = \"branch\")", n, r.MainBranch)
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
	for _, id := range slices.Sorted(maps.Keys(c.Providers)) {
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
		for _, r := range p.Roles {
			if !slices.Contains(providerRoles, r) {
				bad("providers.%s.roles: %q is not one of %v", id, r, providerRoles)
			}
		}
		if p.BudgetUSDDay < 0 {
			bad("providers.%s.budget_usd_day must be >= 0", id)
		}
		if p.Kind == "mock" && len(p.Command) == 0 {
			bad("providers.%s: kind mock needs command", id)
		}
		for _, t := range p.Tiers {
			if !slices.Contains(tiers, t) {
				bad("providers.%s.tiers: %q is not one of %v", id, t, tiers)
			}
		}
		for _, k := range slices.Sorted(maps.Keys(p.Env)) {
			if secretKey.MatchString(k) && !refPattern.MatchString(p.Env[k]) {
				bad("providers.%s.env.%s holds a value; use secret:<name> or env:<VAR>", id, k)
			}
		}
	}
	for _, n := range slices.Sorted(maps.Keys(c.Nodes)) {
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
	for _, n := range slices.Sorted(maps.Keys(c.MCP)) {
		m := c.MCP[n]
		if n == "board" {
			bad("mcp.board is reserved for the swarm board")
		}
		if m.Command == "" {
			bad("mcp.%s.command is required", n)
		}
		if c.Strict() && (len(m.Roles) == 0 || slices.Contains(m.Roles, "worker")) && len(m.Tools) == 0 {
			bad("mcp.%s.tools is required for a server workers get (it runs outside their sandbox)", n)
		}
		for _, r := range m.Roles {
			if r != "worker" && r != "lead" {
				bad("mcp.%s.roles: %q is not worker or lead", n, r)
			}
		}
		for _, k := range slices.Sorted(maps.Keys(m.Env)) {
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

// resolvePaths expands ~ and makes profile-relative paths absolute, once, so every
// consumer reads final paths.
func (c *Config) resolvePaths() {
	abs := func(p string) string {
		if p == "" {
			return p
		}
		p = Expand(p)
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.Dir, p)
		}
		return filepath.Clean(p)
	}
	c.Profile.StateDir = abs(c.Profile.StateDir)
	c.Profile.WorktreeRoot = abs(c.Profile.WorktreeRoot)
	c.Profile.ProjectRules = abs(c.Profile.ProjectRules)
	for i, p := range c.Profile.DenyPaths {
		c.Profile.DenyPaths[i] = abs(p)
	}
	for n, r := range c.Repos {
		r.Path = abs(r.Path)
		c.Repos[n] = r
	}
	for n, p := range c.Providers {
		p.ConfigDir, p.Home, p.LimitsFile = Expand(p.ConfigDir), Expand(p.Home), Expand(p.LimitsFile)
		if p.PauseAbove == 0 {
			p.PauseAbove = 80
		}
		c.Providers[n] = p
	}
	for n, m := range c.MCP {
		m.Command = Expand(m.Command)
		c.MCP[n] = m
	}
	if c.Sandbox.AllowWrite == nil { // build caches of the usual toolchains
		home, _ := os.UserHomeDir()
		for _, d := range []string{"Library/Caches/go-build", ".cache", "go/pkg/mod", ".npm", ".cargo/registry"} {
			c.Sandbox.AllowWrite = append(c.Sandbox.AllowWrite, filepath.Join(home, d))
		}
	}
	for i, p := range c.Sandbox.AllowWrite {
		c.Sandbox.AllowWrite[i] = Expand(p)
	}
}

func (c *Config) StatePath() string { return c.Profile.StateDir }

func (c *Config) DBPath() string { return filepath.Join(c.StatePath(), "swarm.db") }

// Schema is the JSON Schema of swarm.toml, inferred from these types, so it cannot drift
// from what Load accepts. Unknown keys are refused, as Load refuses them.
func Schema() (*jsonschema.Schema, error) {
	s, err := jsonschema.For[Config](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[Duration](): {Type: "string", Pattern: `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`,
			Description: "a Go duration: 90s, 15m, 1h30m"},
	}})
	if err != nil {
		return nil, err
	}
	s.Title = "swarm.toml"
	s.Schema = "https://json-schema.org/draft/2020-12/schema"
	return s, nil
}

// Can: whether a provider may serve a role. No roles listed means a worker only.
func (p Provider) Can(role string) bool {
	if len(p.Roles) == 0 {
		return role == "worker"
	}
	return slices.Contains(p.Roles, role)
}

// Reruns of a failed gate command; 1 unless the repo says otherwise.
func (r Repo) Reruns() int {
	if r.GateReruns == nil {
		return 1
	}
	return *r.GateReruns
}

// WorkerDeny: paths a worker may neither read nor edit. The profile's state (operator token,
// database, attempt dirs with resolved MCP env), every provider's account dir, the usual
// key stores, and whatever the profile adds.
func (c *Config) WorkerDeny() []string {
	home, _ := os.UserHomeDir()
	out := []string{c.Profile.StateDir}
	for _, p := range c.Providers {
		for _, d := range []string{p.ConfigDir, p.Home} {
			if d != "" {
				out = append(out, d)
			}
		}
	}
	for _, d := range []string{".ssh", ".aws", ".gnupg", ".netrc", ".config/gh", ".docker", ".kube", "Library/Keychains", ".swarm/accounts"} {
		out = append(out, filepath.Join(home, d))
	}
	out = append(out, c.Profile.DenyPaths...)
	slices.Sort(out)
	return slices.Compact(out)
}

// Strict: the profile treats workers as untrusted (see Profile.Safety).
func (c *Config) Strict() bool { return c.Profile.Safety == "strict" }

// MainRef: the ref tasks branch from and are measured against. main_branch is a branch name
// ("main") or, for a host that bases work on a ref someone else moves, a full ref
// ("refs/remotes/laptop/main"); the latter only with merge = "branch".
func (r Repo) MainRef() string {
	if strings.HasPrefix(r.MainBranch, "refs/") {
		return r.MainBranch
	}
	return "refs/heads/" + r.MainBranch
}
