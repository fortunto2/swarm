package provider

import (
	"strings"
	"testing"

	"github.com/fortunto2/swarm/internal/config"
)

func TestParseUsageTakesTheLastResult(t *testing.T) {
	log := `{"type":"system","subtype":"init"}
{"type":"result","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2}}
{"type":"assistant"}
{"type":"result","total_cost_usd":0.067,"usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":42}}`
	u := ParseUsage(log)
	if !u.Known || u.TokensIn != 115 || u.TokensOut != 42 || u.CostUSD != 0.067 {
		t.Fatalf("%+v", u)
	}
	if ParseUsage(`{"type":"assistant"}`).Known {
		t.Fatal("no result line must be unknown, not zero")
	}
}

func TestAllowedToolsNoPush(t *testing.T) {
	a := Attempt{Gate: []string{"go vet ./...", "go test ./...", "make lint"},
		ExtraMCP: map[string]config.MCPServer{"solograph": {Command: "x"}}}
	got := strings.Join(AllowedTools(a, []string{"Bash(npm ci:*)"}), " ")
	for _, want := range []string{"mcp__board__*", "mcp__solograph__*", "Bash(go:*)", "Bash(make:*)", "Bash(npm ci:*)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "push") || strings.Contains(got, "Bash(git:*)") {
		t.Fatalf("push reachable: %s", got)
	}
}

func TestCleanEnvDropsAccountVars(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/other/account")
	t.Setenv("ANTHROPIC_API_KEY", "x")
	for _, kv := range CleanEnv() {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") || strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Fatalf("leaked %s", kv)
		}
	}
}
