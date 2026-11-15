package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
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
		"passwd key":       {`ANTHROPIC_AUTH_TOKEN = "secret:kimi"`, `DB_PASSWD = "hunter2"`, "holds a value"},
		"cookie key":       {`ANTHROPIC_AUTH_TOKEN = "secret:kimi"`, `SESSION_COOKIE = "abc"`, "holds a value"},
		"dsn key":          {`ANTHROPIC_AUTH_TOKEN = "secret:kimi"`, `DATABASE_DSN = "postgres://u:p@h/db"`, "holds a value"},
		"empty ref":        {`"secret:kimi"`, `"secret:"`, "holds a value"},
		"worktree in repo": {`worktree_root = "/tmp/swarm-wt"`, `worktree_root = "/tmp/app/.swarm/wt"`, "inside repos.app"},
		"bad merge":        {`merge = "ff"`, `merge = "push"`, "merge"},
		"empty gate":       {`gate = ["go test ./..."]`, `gate = []`, "gate is empty"},
		"unknown kind":     {`kind = "claude"`, `kind = "opencode"`, "kind"},
		"unknown account":  {`accounts = ["claude-a"]`, `accounts = ["nope"]`, "not a provider"},
		"unknown key":      {`slots = 2`, "slots = 2\nslot = 3", "unknown keys"},
		"branch on worker": {`merge = "ff"`, "merge = \"branch\"\nbranch_prefix = \"swarm/\"", "the worker's own worktree"},
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

// The shipped example validates against the inferred schema, and the schema refuses what
// Load refuses: an unknown key. A schema that drifts from the types fails here.
func TestSchemaMatchesLoad(t *testing.T) {
	s, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	asJSON := func(body string) any {
		var m map[string]any
		if _, err := toml.Decode(body, &m); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(m)
		var v any
		_ = json.Unmarshal(b, &v)
		return v
	}
	example, err := os.ReadFile("../../configs/swarm.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Validate(asJSON(string(example))); err != nil {
		t.Fatalf("example rejected by the schema: %v", err)
	}
	if err := rs.Validate(asJSON(strings.Replace(good, "slots = 2", "slots = 2\nslot = 3", 1))); err == nil {
		t.Fatal("schema accepted an unknown key that Load refuses")
	}
	if err := rs.Validate(asJSON(strings.Replace(string(example), `task_wall_clock = "45m"`, `task_wall_clock = "soon"`, 1))); err == nil {
		t.Fatal("schema accepted a duration Load cannot parse")
	}
}

// The selftest profile is written to disk and loaded like any other: it keeps one provider
// with its secrets as references, the nodes that may run it, the safety setting, and
// nothing that reaches outside its temp dir.
func TestSelftestProfileRoundTrips(t *testing.T) {
	src := strings.Replace(good, "[nodes.laptop]", `[providers.mock-b]
kind = "mock"
family = "mock"
slots = 1
command = ["/bin/true"]

[hooks]
after_merge = ["echo merged"]

[mcp.index]
command = "index"
tools = ["search"]

[nodes.other]
host = "remote"
accounts = ["mock-b"]

[nodes.laptop]`, 1)
	src = strings.Replace(src, `worktree_root = "/tmp/swarm-wt"`, "worktree_root = \"/tmp/swarm-wt\"\nsafety = \"strict\"\nnotify = \"say\"\nsecret_cmd = [\"keys\", \"get\"]", 1)
	c, err := Load(write(t, src))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, err := c.Selftest("claude-a", dir, filepath.Join(dir, "repo"), 7*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := st.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"secret:kimi"`) {
		t.Fatalf("the key reference did not survive encoding:\n%s", buf.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "swarm.toml"), []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("the encoded selftest profile does not load: %v\n%s", err, buf.String())
	}
	if len(got.Providers) != 1 || got.Providers["claude-a"].Env["ANTHROPIC_AUTH_TOKEN"] != "secret:kimi" {
		t.Fatalf("providers: %+v", got.Providers)
	}
	if _, ok := got.Nodes["other"]; ok || !slices.Equal(got.Nodes["laptop"].Accounts, []string{"claude-a"}) {
		t.Fatalf("nodes: %+v", got.Nodes)
	}
	if !got.Strict() || !slices.Equal(got.Profile.SecretCmd, []string{"keys", "get"}) {
		t.Fatalf("safety or secret_cmd not carried: %+v", got.Profile)
	}
	if got.Profile.Notify != "" || len(got.Hooks.AfterMerge) != 0 || len(got.MCP) != 0 {
		t.Fatalf("something reaching outside the temp dir came along: %+v %+v %+v", got.Profile, got.Hooks, got.MCP)
	}
	r := got.Repos[SelftestRepo]
	if len(got.Repos) != 1 || r.Merge != "ff" || !slices.Equal(r.Gate, SelftestGate) || r.Path != filepath.Join(dir, "repo") {
		t.Fatalf("repos: %+v", got.Repos)
	}
	if got.Limits.MaxWorkers != 1 || got.Limits.TaskWallClock.Duration != 7*time.Minute ||
		!strings.HasPrefix(got.StatePath(), dir) || !strings.HasPrefix(got.Profile.WorktreeRoot, dir) {
		t.Fatalf("limits or paths: %+v %+v", got.Limits, got.Profile)
	}
	if _, err := c.Selftest("nope", dir, dir, time.Minute); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
}

// configs/swarm.schema.json is what editors read; it must be the generated one.
func TestCommittedSchemaIsCurrent(t *testing.T) {
	s, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.MarshalIndent(s, "", "  ")
	got, err := os.ReadFile("../../configs/swarm.schema.json")
	if err != nil || strings.TrimSpace(string(got)) != string(want) {
		t.Fatal("configs/swarm.schema.json is stale: run `make schema`")
	}
}
