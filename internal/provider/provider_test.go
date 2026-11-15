package provider

import (
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
		ExtraMCP: map[string]config.MCPServer{"solograph": {Command: "x"}}}
	got := strings.Join(AllowedTools(a, []string{"Bash(npm ci:*)"}), " ")
	for _, want := range []string{"mcp__board__*", "mcp__solograph__*", "Bash(go:*)", "Bash(make:*)", "Bash(npm ci:*)", "Bash(swarm gate:*)"} {
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
