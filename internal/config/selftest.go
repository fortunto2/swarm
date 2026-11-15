package config

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"

	"github.com/BurntSushi/toml"
)

// SelftestRepo is the one repo of a selftest profile: a throwaway git repo in its temp dir.
const SelftestRepo = "selftest"

// SelftestGate needs nothing but sh: the answer file holds exactly 42.
var SelftestGate = []string{`test "$(cat answer.txt)" = 42`}

// Selftest derives the profile `swarm selftest` runs in dir: provider id alone (the run
// proves that one, not whichever answers first), the nodes that may run it, the safety
// settings and the secret helpers, and one repo at repoPath. Nothing that reaches outside
// dir comes along: no hooks, no notifier, no extra MCP servers, no project rules, no harness.
// Provider env stays as written (secret:/env: references), so Encode never puts a resolved
// value on disk.
func (c *Config) Selftest(id, dir, repoPath string, wall time.Duration) (*Config, error) {
	p, ok := c.Providers[id]
	if !ok {
		return nil, fmt.Errorf("profile has no provider %q", id)
	}
	nodes := map[string]Node{}
	for n, nd := range c.Nodes {
		if len(nd.Accounts) > 0 {
			if !slices.Contains(nd.Accounts, id) {
				continue // a node that never runs this provider has no place here
			}
			nd.Accounts = []string{id} // the others are not providers of this profile
		}
		nodes[n] = nd
	}
	noReruns := 0 // the check is deterministic: a second run would say the same
	return &Config{
		Profile: Profile{
			Name:         c.Profile.Name + "-selftest",
			StateDir:     filepath.Join(dir, "state"),
			WorktreeRoot: filepath.Join(dir, "wt"),
			Safety:       c.Profile.Safety,
			SecretCmd:    c.Profile.SecretCmd,
			SecretExec:   c.Profile.SecretExec,
			Redact:       c.Profile.Redact,
			DenyPaths:    c.Profile.DenyPaths,
		},
		Repos: map[string]Repo{SelftestRepo: {Path: repoPath, MainBranch: "main", Gate: SelftestGate,
			GateTimeout: Duration{time.Minute}, Merge: "ff", GateReruns: &noReruns}},
		// one attempt: a gate that fails on "write 42" is the answer, not something to retry
		Limits:    Limits{MaxWorkers: 1, MaxAttempts: 1, MaxGates: 1, TaskWallClock: Duration{wall}},
		Providers: map[string]Provider{id: p},
		Nodes:     nodes,
		Sandbox:   c.Sandbox,
	}, nil
}

// Encode writes c as swarm.toml: Load reads back what was encoded.
func (c *Config) Encode(w io.Writer) error { return toml.NewEncoder(w).Encode(c) }
