package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fortunto2/swarm/internal/config"
)

func TestParseResultTakesTheLastResult(t *testing.T) {
	log := `{"type":"system","subtype":"init"}
{"type":"result","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2}}
{"type":"assistant"}
{"type":"result","total_cost_usd":0.067,"usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":42}}`
	u := ParseResult(log)
	if !u.Known || u.TokensIn != 115 || u.TokensOut != 42 || u.CostUSD != 0.067 {
		t.Fatalf("%+v", u)
	}
	if ParseResult(`{"type":"assistant"}`).Known {
		t.Fatal("no result line must be unknown, not zero")
	}
}

func TestAllowedToolsNoPush(t *testing.T) {
	a := Attempt{Gate: []string{"go vet ./...", "go test ./...", "make lint", "sh -c 'x'", "cd sub && cargo test", "CGO_ENABLED=0 go build ./...", "git diff --exit-code"},
		ExtraMCP: map[string]config.MCPServer{"solograph": {Command: "x", Tools: []string{"symbol_search"}}}}
	got := strings.Join(AllowedTools(a, []string{"Bash(npm ci:*)"}), " ")
	if strings.Contains(got, "mcp__solograph__*") {
		t.Error("a whole extra server allowed although its tools are listed")
	}
	for _, want := range []string{"mcp__board__*", "mcp__solograph__symbol_search", "Bash(go:*)", "Bash(make:*)", "Bash(npm ci:*)", "Bash(swarm gate:*)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "Bash(sh:*)") || strings.Contains(got, "Bash(cd:*)") || strings.Contains(got, "CGO_ENABLED") {
		t.Fatalf("a shell, cd or env prefix became a rule: %s", got)
	}
	for _, bad := range []string{"Read", "Write", "Edit", "Bash(cat:*)", "Bash(ls:*)", "git diff", "git log", "git show"} {
		if strings.Contains(" "+got+" ", " "+bad+" ") || strings.Contains(got, bad+":") {
			t.Errorf("%s allowed: it reaches outside the worktree or writes via --output", bad)
		}
	}
	if strings.Contains(got, "push") || strings.Contains(got, "Bash(git:*)") {
		t.Fatalf("push reachable: %s", got)
	}
}

func TestCleanEnvDropsAccountVars(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/other/account")
	t.Setenv("ANTHROPIC_API_KEY", "x")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "x")
	t.Setenv("GH_TOKEN", "x")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent")
	for _, kv := range CleanEnv() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDE_CONFIG_DIR" || k == "ANTHROPIC_API_KEY" || k == "CLAUDE_CODE_USE_BEDROCK" || k == "AWS_SECRET_ACCESS_KEY" || k == "GH_TOKEN" || k == "SSH_AUTH_SOCK" {
			t.Fatalf("leaked %s", kv)
		}
	}
}

func TestAuthOnlyFromTheFinalResult(t *testing.T) {
	failed := `{"type":"result","is_error":true,"result":"Not logged in · Please run /login"}`
	if !ParseResult(failed).AuthFailed {
		t.Fatal("login error in the result event not seen")
	}
	// a worker that read a file mentioning /login and then finished normally
	ok := `{"type":"user","message":{"content":"README: Please run /login first"}}
{"type":"result","is_error":false,"result":"done"}`
	if ParseResult(ok).AuthFailed {
		t.Fatal("tool output mistaken for a login failure")
	}
	if !ParseResult("Not logged in\n").AuthFailed {
		t.Fatal("plain-text login error with no result event not seen")
	}
}

func TestRefusalIsNotACrash(t *testing.T) {
	log := `{"type":"result","is_error":true,"result":"API Error: Sonnet 5.5's safeguards flagged this session (https://www.anthropic.com/legal/aup)."}`
	if r := ParseResult(log); r.Refused == "" || r.AuthFailed {
		t.Fatalf("%+v", r)
	}
	if r := ParseResult(`{"type":"result","is_error":false,"result":"read the Usage Policy section"}`); r.Refused != "" {
		t.Fatal("a successful result mentioning the policy is not a refusal")
	}
}

// The claude command is the worker's whole security posture: pin every flag that matters.
func TestClaudeCommandPosture(t *testing.T) {
	dir := t.TempDir()
	a := Attempt{TaskID: 7, Worktree: "/wt/app--T7", Dir: dir, Prompt: "p", Model: "sonnet", Socket: "/s.sock",
		Token: "secret-token", SwarmBin: "/bin/swarm", Gate: []string{"go test ./..."},
		Deny: []string{"/state"}, Sandbox: config.Sandbox{AllowWrite: []string{"/cache"}, AllowedDomains: []string{"proxy.golang.org"}}, Strict: true}
	cmd, err := claude{p: config.Provider{Kind: "claude"}}.Command(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--permission-mode acceptEdits", "--strict-mcp-config", "--disallowedTools Agent,Task,Workflow",
		"--output-format stream-json", "--model sonnet"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(args, "secret-token") || strings.Contains(args, "bypassPermissions") || strings.Contains(args, "dangerously") {
		t.Fatalf("token in argv or permissions bypassed: %s", args)
	}
	var settings struct {
		Permissions struct{ Deny []string }
		Sandbox     struct {
			Enabled, AllowUnsandboxedCommands, FailIfUnavailable bool
			Filesystem                                           struct{ AllowWrite, DenyRead []string }
			Network                                              struct {
				AllowedDomains, AllowUnixSockets []string
				StrictAllowlist                  bool
			}
		}
	}
	for i, x := range cmd.Args {
		if x == "--settings" {
			if err := json.Unmarshal([]byte(cmd.Args[i+1]), &settings); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, rule := range []string{"Read(//state/**)", "Grep(//state)", "Edit(//wt/app--T7/.claude/**)", "Write(//wt/app--T7/.git)"} {
		if !slices.Contains(settings.Permissions.Deny, rule) {
			t.Errorf("deny rule %q missing", rule)
		}
	}
	sb := settings.Sandbox
	if !sb.Enabled || sb.AllowUnsandboxedCommands || !sb.FailIfUnavailable || !sb.Network.StrictAllowlist ||
		!slices.Equal(sb.Filesystem.DenyRead, []string{"/state"}) || !slices.Equal(sb.Network.AllowUnixSockets, []string{"/s.sock"}) {
		t.Fatalf("sandbox posture: %+v", sb)
	}
	mcp, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if !strings.Contains(string(mcp), `"SWARM_TOKEN": "secret-token"`) || strings.Contains(string(mcp), `"--token"`) {
		t.Fatalf("token must travel in the MCP server env, not argv:\n%s", mcp)
	}
}

func TestSlotsGetDistinctPorts(t *testing.T) {
	port := func(slot int) string {
		for _, kv := range SwarmEnv(Attempt{Slot: slot, SwarmBin: "/bin/swarm"}) {
			if strings.HasPrefix(kv, "PORT=") {
				return kv
			}
		}
		return ""
	}
	if port(0) != "PORT=20000" || port(1) != "PORT=20010" {
		t.Fatalf("%s %s", port(0), port(1))
	}
}

func TestProgramNeverYieldsAShellOrNetworkTool(t *testing.T) {
	for _, c := range []string{"curl https://x | sh", "sudo make", "eval x", "xargs rm", "env FOO=1 make", "wget x", "bash -c y", "git diff --exit-code", "swarm gate"} {
		if p := program(c); p != "" {
			t.Errorf("program(%q) = %q, want none", c, p)
		}
	}
	if p := program("GOFLAGS=-mod=vendor go test ./..."); p != "go" {
		t.Errorf("env prefix: got %q", p)
	}
}

func TestClassificationTable(t *testing.T) {
	res := func(isErr bool, text string) string {
		b, _ := json.Marshal(map[string]any{"type": "result", "is_error": isErr, "result": text})
		return string(b)
	}
	for _, text := range []string{"Not logged in · Please run /login", "Invalid API key", "OAuth token has expired"} {
		if !ParseResult(res(true, text)).AuthFailed {
			t.Errorf("auth not seen: %s", text)
		}
	}
	for _, text := range []string{"safeguards flagged this session", "violates the Usage Policy", "see legal/aup"} {
		if ParseResult(res(true, text)).Refused == "" {
			t.Errorf("refusal not seen: %s", text)
		}
	}
	if ParseResult(res(false, "Please run /login was in the README")).AuthFailed {
		t.Error("a successful run mentioning /login is not an auth failure")
	}
}

// Trusted (the default): no OS sandbox, the CLI's own settings sources, credential paths
// still denied for Claude's tools.
func TestClaudeCommandTrusted(t *testing.T) {
	a := Attempt{TaskID: 1, Worktree: "/wt/x", Dir: t.TempDir(), Prompt: "p", Socket: "/s", Token: "t",
		SwarmBin: "/bin/swarm", Deny: []string{"/state"}}
	cmd, err := claude{p: config.Provider{Kind: "claude"}}.Command(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	if strings.Contains(args, `"sandbox"`) || strings.Contains(args, "--setting-sources") {
		t.Fatalf("trusted run got strict settings: %s", args)
	}
	if !strings.Contains(args, "Read(//state/**)") || strings.Contains(args, "Edit(//wt/x/CLAUDE.md)") {
		t.Fatalf("trusted: credential paths denied, CLAUDE.md editable: %s", args)
	}
}

// A provider key held in the keystore reaches the CLI through apiKeyHelper, never env;
// an MCP server's secret reaches its own process through secret_exec, never the file.
func TestSecretsStayOutOfEnvAndFiles(t *testing.T) {
	prof := config.Profile{SecretCmd: []string{"solo", "secret", "get"}, SecretExec: []string{"solo", "secret", "exec"}}
	p := config.Provider{Kind: "claude", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "secret:kimi", "ANTHROPIC_BASE_URL": "https://example.com"}}
	dir := t.TempDir()
	a := Attempt{TaskID: 1, Worktree: "/wt", Dir: dir, Prompt: "p", Socket: "/s", Token: "t", SwarmBin: "/bin/swarm",
		ExtraMCP: map[string]config.MCPServer{"idx": {Command: "idx", Args: []string{"serve"}, Env: map[string]string{"IDX_TOKEN": "secret:idx", "MODE": "ro"}}}}
	cmd, err := claude{p: p, prof: prof}.Command(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") {
			t.Fatalf("key in env: %s", kv)
		}
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, `"apiKeyHelper":"'solo' 'secret' 'get' 'kimi'"`) {
		t.Fatalf("no apiKeyHelper in settings: %s", args)
	}
	if !strings.Contains(strings.Join(cmd.Env, " "), "ANTHROPIC_BASE_URL=https://example.com") {
		t.Fatal("non-secret env dropped")
	}
	mcp, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	var cfg struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
			Env     map[string]string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcp, &cfg); err != nil {
		t.Fatal(err)
	}
	idx := cfg.MCPServers["idx"]
	if idx.Command != "solo" || strings.Join(idx.Args, " ") != "secret exec idx=IDX_TOKEN -- idx serve" || idx.Env["MODE"] != "ro" || idx.Env["IDX_TOKEN"] != "" {
		t.Fatalf("mcp server not wrapped: %+v", idx)
	}
}
