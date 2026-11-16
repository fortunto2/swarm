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

	"github.com/fortunto2/swarm/internal/jobs"
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
	Harness      string   `toml:"harness" json:"harness,omitempty"`             // accepted so older profiles still load; nothing reads it (the harness contract goes through hooks, docs/hardening.md)
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
	GateReruns   *int   `toml:"gate_reruns" json:"gate_reruns,omitempty"`
	// GateOn: where the gate commands run. "" or "coordinator": here, in a throwaway
	// worktree; "node": on the node whose worker made the commit (its machine may be the
	// one with the cores, the toolchain or the data the suite needs), waiting while that node
	// is away rather than run here, where the suite may not even start; "<a remote node>":
	// on that node while it is connected and has the repo, whoever made the commit, else here
	// (a box's tasks gated on a laptop's cores, measured 2 min there against 15-25 here).
	GateOn string `toml:"gate_on" json:"gate_on,omitempty"`
	// Review: a provider that reads each change after the gate's commands pass, read-only, and
	// approves or rejects it (internal/review). It runs where the gate runs, on that machine's
	// account of that id. "" = no review.
	Review string `toml:"review" json:"review,omitempty"`

	// Node profiles only: how this node keeps its copy in step with the coordinator's.
	Remote  string    `toml:"remote" json:"remote,omitempty"`   // git URL of the coordinator's copy, as this node reaches it
	Publish []RefSync `toml:"publish" json:"publish,omitempty"` // refs pushed to the coordinator's copy when they move (e.g. its main)
	Fetch   string    `toml:"fetch" json:"fetch,omitempty"`     // a remote fetched every few minutes before publishing (e.g. origin)
	Land    string    `toml:"land" json:"land,omitempty"`       // "ff": fast-forward this node's main to the coordinator's after a merge // reruns of a failed gate command (flaky tests); default 1 // "ff": ff-only into main; "pr": leave a branch for a human PR
}

// RefSync: one ref a node pushes to the coordinator's copy of a repo.
type RefSync struct {
	From string `toml:"from" json:"from"` // a ref here, e.g. refs/remotes/origin/main
	To   string `toml:"to" json:"to"`     // the ref there, e.g. refs/remotes/laptop/main
}

type Limits struct {
	MaxWorkers    int      `toml:"max_workers" json:"max_workers,omitempty"`
	TaskWallClock Duration `toml:"task_wall_clock" json:"task_wall_clock,omitempty"`
	MaxAttempts   int      `toml:"max_attempts" json:"max_attempts,omitempty"`
	MaxGates      int      `toml:"max_gates" json:"max_gates,omitempty"` // gates running at once; default half of max_workers
	// NodeTimeout: a remote node not heard from for this long is offline (no new work);
	// for twice as long, its running attempts count as lost and go back to ready. 90s.
	NodeTimeout Duration `toml:"node_timeout" json:"node_timeout,omitempty"`
	// Admit: a shell command asked, off the loop and at most every 15 s, before new workers
	// or gates start. Exit 0 admits; anything else holds them, and the first output line is
	// the reason. For a host shared with other work: hold while it runs, or while memory is low.
	Admit string `toml:"admit" json:"admit,omitempty"`
	// EscalateAfter: a tier-B task that failed this many attempts in a row (gate, review, wall
	// clock) is taken by a tier-A provider and model from then on. Default 2 (the third attempt
	// is the stronger one); negative never escalates.
	EscalateAfter int `toml:"escalate_after" json:"escalate_after,omitempty"`
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

// Node: a machine workers run on. "local" is the one the daemon runs on; "remote" is one
// that dials in with `swarm node` (a laptop, a box behind NAT, a VPS) and runs the workers
// of its accounts there. Accounts: the providers logged in on that machine. Labels: what a
// task's runs_on can ask for (vpc, gpu, mac).
type Node struct {
	Host       string   `toml:"host" json:"host,omitempty"` // "local" or "remote"
	MaxWorkers int      `toml:"max_workers" json:"max_workers,omitempty"`
	Accounts   []string `toml:"accounts" json:"accounts,omitempty"`
	Roles      []string `toml:"roles" json:"roles,omitempty"`
	Labels     []string `toml:"labels" json:"labels,omitempty"`
	Repos      []string `toml:"repos" json:"repos,omitempty"` // the repos it works on; empty = every one it has (a toolchain may be missing elsewhere)
}

// NodeSelf: in a node profile, who this node is and how it reaches the coordinator. The
// command runs swarm on the coordinator's host with the coordinator's profile; its stdin
// and stdout become the link (ssh is the usual transport, and also the authentication).
type NodeSelf struct {
	Name        string   `toml:"name" json:"name"`
	Coordinator []string `toml:"coordinator" json:"coordinator"`
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
	Dir       string              `toml:"-" json:"-"` // the profile directory this file was read from
	Profile   Profile             `toml:"profile" json:"profile,omitempty"`
	Repos     map[string]Repo     `toml:"repos" json:"repos,omitempty"`
	Limits    Limits              `toml:"limits" json:"limits,omitempty"`
	Providers map[string]Provider `toml:"providers" json:"providers,omitempty"`
	Nodes     map[string]Node     `toml:"nodes" json:"nodes,omitempty"`
	Node      *NodeSelf           `toml:"node" json:"node,omitempty"` // set in a node profile only

	raw     map[string]Provider  // providers as written: a remote node expands ~ and resolves secrets itself
	MCP     map[string]MCPServer `toml:"mcp" json:"mcp,omitempty"`
	Hooks   Hooks                `toml:"hooks" json:"hooks,omitempty"`
	Sandbox Sandbox              `toml:"sandbox" json:"sandbox,omitempty"`
	Jobs    map[string]Job       `toml:"jobs" json:"jobs,omitempty"`
}

// Job: a shell command the coordinator runs on a schedule (internal/jobs). Missed while the
// daemon was down, it runs once when it is back; a new job waits for its first scheduled
// minute. Its exit and the tail of its output are kept; a failure is told through notify.
type Job struct {
	Cron    string   `toml:"cron" json:"cron"`                 // five fields, UTC: "0 14,21 * * 1-5"
	Run     string   `toml:"run" json:"run"`                   // sh -c, with the daemon's environment (the operator's command)
	Repo    string   `toml:"repo" json:"repo,omitempty"`       // run in that repo's directory; else the profile's
	Timeout Duration `toml:"timeout" json:"timeout,omitempty"` // default 30m: then the whole group is killed
	Notify  string   `toml:"notify" json:"notify,omitempty"`   // fail (default) | always | never
	// Spare: run only while limits.admit admits; held otherwise (a heavy job on a shared host).
	Spare bool `toml:"spare" json:"spare,omitempty"`
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
	c.raw = maps.Clone(c.Providers)
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
	if c.Limits.EscalateAfter == 0 {
		c.Limits.EscalateAfter = 2
	}
	if c.Limits.MaxAttempts == 0 {
		c.Limits.MaxAttempts = 4
	}
	if c.Limits.MaxGates == 0 {
		c.Limits.MaxGates = max(1, c.Limits.MaxWorkers/2)
	}
	if c.Limits.NodeTimeout.Duration == 0 {
		c.Limits.NodeTimeout.Duration = 90 * time.Second
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
	if c.Node != nil {
		return c.validateNode()
	}
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }

	if c.Profile.Name == "" {
		bad("profile.name is required")
	}
	for name, j := range c.Jobs {
		if _, err := jobs.Parse(j.Cron); err != nil {
			bad("jobs.%s: %v", name, err)
		}
		if strings.TrimSpace(j.Run) == "" {
			bad("jobs.%s.run is empty", name)
		}
		if _, ok := c.Repos[j.Repo]; j.Repo != "" && !ok {
			bad("jobs.%s.repo = %q: no such repo", name, j.Repo)
		}
		if !slices.Contains([]string{"", "fail", "always", "never"}, j.Notify) {
			bad("jobs.%s.notify = %q, want fail, always or never", name, j.Notify)
		}
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
		if r.GateOn != "" && r.GateOn != "coordinator" && r.GateOn != "node" && c.Nodes[r.GateOn].Host != "remote" {
			bad("repos.%s.gate_on = %q, want coordinator, node, or the name of a remote node", n, r.GateOn)
		}
		if r.Review != "" {
			if p, ok := c.Providers[r.Review]; !ok {
				bad("repos.%s.review = %q: no such provider", n, r.Review)
			} else if p.Kind != "claude" && p.Kind != "codex" && p.Kind != "mock" {
				bad("repos.%s.review = %q: kind %q cannot review", n, r.Review, p.Kind)
			}
			// a gate on a named node reviews with that node's account of this id
			if g, ok := c.Nodes[r.GateOn]; ok && g.Host == "remote" && !slices.Contains(g.Accounts, r.Review) {
				bad("repos.%s.review = %q: its gate runs on node %s, which has no such account (add it to nodes.%s.accounts)", n, r.Review, r.GateOn, r.GateOn)
			}
		}
		if r.Remote != "" || len(r.Publish) > 0 || r.Fetch != "" || r.Land != "" {
			bad("repos.%s: remote, publish, fetch and land belong in a node profile ([node]), not the coordinator's", n)
		}
		if r.Merge == "branch" && r.BranchPrefix+"T1" == WorkerBranch(1) {
			bad("repos.%s.branch_prefix = %q delivers T<n> to %s, the branch the worker's own worktree holds: git refuses to move it (use the default %q)",
				n, r.BranchPrefix, WorkerBranch(1)[:len(WorkerBranch(1))-1]+"<n>", "swarm/done/")
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
	owner := map[string]string{}
	locals := 0
	for _, n := range slices.Sorted(maps.Keys(c.Nodes)) {
		nd := c.Nodes[n]
		switch nd.Host {
		case "local":
			locals++
		case "remote":
		default:
			bad("nodes.%s.host = %q, want \"local\" (the daemon's machine) or \"remote\" (dials in with swarm node)", n, nd.Host)
		}
		if nd.Host == "remote" && len(nd.Accounts) == 0 {
			bad("nodes.%s: a remote node needs accounts (the providers logged in on it)", n)
		}
		for _, a := range nd.Accounts {
			if _, ok := c.Providers[a]; !ok {
				bad("nodes.%s.accounts: %q is not a provider", n, a)
			}
			if o, dup := owner[a]; dup {
				bad("provider %s is in the accounts of nodes %s and %s: an account runs on one machine", a, o, n)
			}
			owner[a] = n
		}
		for _, r := range nd.Repos {
			if _, ok := c.Repos[r]; !ok {
				bad("nodes.%s.repos: %q is not a repo of this profile", n, r)
			}
		}
		for _, l := range nd.Labels {
			if strings.TrimSpace(l) == "" || strings.ContainsAny(l, ", ") {
				bad("nodes.%s.labels: %q is not a label (one word, no commas)", n, l)
			}
		}
		for _, r := range nd.Roles {
			if !slices.Contains(roles, r) {
				bad("nodes.%s.roles: %q is not one of %v", n, r, roles)
			}
		}
	}
	if locals > 1 {
		bad("more than one node has host = \"local\": the daemon runs on one machine")
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

// validateNode: a node profile says who the node is, how it reaches the coordinator, and
// where its copies of the repos are. Providers, gates and merge rules live with the
// coordinator, which sends them when the node connects.
func (c *Config) validateNode() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if c.Profile.Name == "" {
		bad("profile.name is required")
	}
	if c.Profile.WorktreeRoot == "" {
		bad("profile.worktree_root is required (outside every repo)")
	}
	if c.Node.Name == "" {
		bad("node.name is required: the coordinator's [nodes.<name>] this machine is")
	}
	if len(c.Node.Coordinator) == 0 {
		bad("node.coordinator is required: the command that runs swarm with the coordinator's profile (e.g. [\"ssh\", \"box\", \"swarm\", \"--profile\", \"/srv/swarm\"])")
	}
	if len(c.Repos) == 0 {
		bad("at least one [repos.<name>] is required: the repos this node can work on")
	}
	for _, n := range slices.Sorted(maps.Keys(c.Repos)) {
		r := c.Repos[n]
		if r.Path == "" {
			bad("repos.%s.path is required", n)
		}
		if r.Remote == "" {
			bad("repos.%s.remote is required: the git URL of the coordinator's copy, as this node reaches it", n)
		}
		if r.Land != "" && r.Land != "ff" {
			bad("repos.%s.land = %q, want ff or nothing", n, r.Land)
		}
		for i, ps := range r.Publish {
			if ps.From == "" || !strings.HasPrefix(ps.To, "refs/") {
				bad("repos.%s.publish[%d]: from is a ref here, to is a full ref there (refs/...)", n, i)
			}
		}
		wt, _ := filepath.Abs(Expand(c.Profile.WorktreeRoot))
		rp, _ := filepath.Abs(Expand(r.Path))
		if r.Path != "" && c.Profile.WorktreeRoot != "" && (wt == rp || strings.HasPrefix(wt, rp+string(filepath.Separator))) {
			bad("profile.worktree_root is inside repos.%s: `git add -A` would sweep worktrees into a commit", n)
		}
	}
	if len(c.Providers) > 0 || len(c.Nodes) > 0 {
		bad("a node profile has no providers or nodes: they are declared in the coordinator's profile, which sends this node its accounts")
	}
	return errors.Join(errs...)
}

// RawProvider: a provider as the profile wrote it, before ~ was expanded here. A node gets
// this one: its config_dir and home are paths on that machine, not on this one.
func (c *Config) RawProvider(id string) Provider {
	if p, ok := c.raw[id]; ok {
		if p.PauseAbove == 0 {
			p.PauseAbove = 80
		}
		return p
	}
	return c.Providers[id]
}

// LocalNode: the name of the node the daemon runs as. A profile without nodes, or without
// a local one, runs as "local".
func (c *Config) LocalNode() string {
	for _, n := range slices.Sorted(maps.Keys(c.Nodes)) {
		if c.Nodes[n].Host == "local" {
			return n
		}
	}
	return "local"
}

// NodeOf: the node a provider's account is logged in on. A provider no node lists runs on
// the local one, unless the local node lists its accounts: then it runs nowhere ("").
func (c *Config) NodeOf(provider string) string {
	for _, n := range slices.Sorted(maps.Keys(c.Nodes)) {
		if slices.Contains(c.Nodes[n].Accounts, provider) {
			return n
		}
	}
	if local, ok := c.Nodes[c.LocalNode()]; ok && len(local.Accounts) > 0 {
		return ""
	}
	return c.LocalNode()
}

// Remote: whether a node dials in rather than being the daemon's own machine.
func (c *Config) Remote(node string) bool {
	n, ok := c.Nodes[node]
	return ok && n.Host == "remote"
}

// Labels: every label some node has, for checking a task's runs_on when it is added.
func (c *Config) Labels() map[string]bool {
	out := map[string]bool{}
	for _, n := range c.Nodes {
		for _, l := range n.Labels {
			out[l] = true
		}
	}
	return out
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

// WorkerBranch: the branch a task's worktree is checked out on. A delivery branch
// (merge = "branch") must never be the same name.
func WorkerBranch(taskID int64) string { return fmt.Sprintf("swarm/T%d", taskID) }
