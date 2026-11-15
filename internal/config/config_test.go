package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `
[profile]
name = "test"
worktree_root = "/tmp/swarm-wt"

[repos.app]
path = "/tmp/app"
gate = ["go test ./..."]
merge = "ff"

[providers.claude-a]
kind = "claude"
family = "anthropic"
slots = 2
tiers = ["S", "A", "B"]
env = { ANTHROPIC_AUTH_TOKEN = "secret:kimi" }

[nodes.laptop]
host = "local"
max_workers = 4
accounts = ["claude-a"]
roles = ["supervisor", "tree"]
`

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "swarm.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadGood(t *testing.T) {
	c, err := Load(write(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Repos["app"].MainBranch != "main" || c.Repos["app"].GateTimeout.Minutes() != 15 {
		t.Fatalf("defaults not applied: %+v", c.Repos["app"])
	}
	if c.Limits.MaxAttempts != 4 {
		t.Fatalf("max_attempts default = %d", c.Limits.MaxAttempts)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	b, err := os.ReadFile("../../configs/swarm.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(write(t, string(b))); err != nil {
		t.Fatalf("the shipped example must be valid: %v", err)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"plaintext secret": {`"secret:kimi"`, `"sk-live-123"`, "holds a value"},
		"worktree in repo": {`worktree_root = "/tmp/swarm-wt"`, `worktree_root = "/tmp/app/.swarm/wt"`, "inside repos.app"},
		"bad merge":        {`merge = "ff"`, `merge = "push"`, "merge"},
		"empty gate":       {`gate = ["go test ./..."]`, `gate = []`, "gate is empty"},
		"unknown kind":     {`kind = "claude"`, `kind = "opencode"`, "kind"},
		"unknown account":  {`accounts = ["claude-a"]`, `accounts = ["nope"]`, "not a provider"},
		"unknown key":      {`slots = 2`, "slots = 2\nslot = 3", "unknown keys"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(good, tc.from, tc.to, 1)
			if body == good {
				t.Fatalf("case did not change the config: %q not found", tc.from)
			}
			_, err := Load(write(t, body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestProfileRequired(t *testing.T) {
	t.Setenv("SWARM_PROFILE", "")
	if _, err := ResolveProfileDir(""); err == nil {
		t.Fatal("no flag and no env must be an error, not a guess")
	}
}
