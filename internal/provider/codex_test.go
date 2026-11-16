package provider

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fortunto2/swarm/internal/config"
)

// worktreeOf: a repo with one commit and a linked worktree, as swarm makes them.
func worktreeOf(t *testing.T) (repo, wt string) {
	t.Helper()
	repo, wt = t.TempDir(), filepath.Join(t.TempDir(), "wt")
	for _, a := range [][]string{
		{"-C", repo, "init", "-q", "-b", "main"},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "--allow-empty", "-m", "base"},
		{"-C", repo, "worktree", "add", "-q", "-b", "swarm/T1", wt},
	} {
		if out, err := exec.Command("git", a...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	return repo, wt
}

func TestCodexCommand(t *testing.T) {
	repo, wt := worktreeOf(t)
	a := Attempt{TaskID: 1, Worktree: wt, Dir: t.TempDir(), Prompt: "do the task", Model: "gpt-5.5-codex",
		Socket: "/run/swarm.sock", Token: "tok-secret", SwarmBin: "/opt/swarm",
		ExtraMCP: map[string]config.MCPServer{"my.index": {Command: "/opt/idx", Args: []string{"serve"}, Env: map[string]string{"IDX_ROOT": "/r"}}}}
	t.Setenv("OPENAI_API_KEY", "sk-should-not-reach-the-shell") // an account var: envx drops it outright
	t.Setenv("BILLING_WEBHOOK_SECRET", "whsec-should-not-reach-the-shell")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.autocrlf")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	ad, err := New("codex-me", config.Provider{Kind: "codex", Home: "/accounts/codex-me"}, config.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := ad.Command(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"codex exec --json -C " + wt,
		`approval_policy="never"`,
		"--sandbox danger-full-access", // trusted: unconfined, as a trusted Claude worker's Bash
		`mcp_servers.board.command="/opt/swarm"`,
		`mcp_servers.board.args=["mcp","--socket","/run/swarm.sock"]`,
		`mcp_servers.board.env_vars=["SWARM_TOKEN"]`,
		`mcp_servers."my.index".env_vars=["IDX_ROOT"]`, // a dotted name stays one key
		"shell_environment_policy.ignore_default_excludes=true",
		"-m gpt-5.5-codex",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q:\n%s", want, argv)
		}
	}
	var exclude string
	for i, s := range cmd.Args {
		if strings.HasPrefix(s, "shell_environment_policy.exclude=") && cmd.Args[i-1] == "-c" {
			exclude = s
		}
	}
	if !strings.Contains(exclude, `"BILLING_WEBHOOK_SECRET"`) || strings.Contains(exclude, "GIT_CONFIG_KEY_0") {
		t.Errorf("shell exclude %q: want the secret out and git's GIT_CONFIG_KEY_n kept (git fails without it)", exclude)
	}
	if strings.Contains(strings.Join(cmd.Env, "\n"), "sk-should-not") {
		t.Error("an account key reached the worker's environment")
	}
	if !strings.Contains(exclude, `"IDX_ROOT"`) || strings.Contains(exclude, "SWARM_TOKEN") {
		t.Errorf("shell exclude %q: want the MCP server's env out and SWARM_TOKEN in (swarm gate needs it)", exclude)
	}
	if strings.Contains(argv, "tok-secret") || strings.Contains(argv, "do the task") || strings.Contains(argv, "should-not-reach") {
		t.Fatalf("a token, a key or the prompt is in argv, which ps shows to anyone:\n%s", argv)
	}
	if cmd.Args[len(cmd.Args)-1] != "-" {
		t.Fatalf("the prompt must come from stdin: last arg %q", cmd.Args[len(cmd.Args)-1])
	}
	if in, _ := io.ReadAll(cmd.Stdin); string(in) != "do the task" {
		t.Fatalf("stdin = %q", in)
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"CODEX_HOME=/accounts/codex-me", "SWARM_TOKEN=tok-secret", "IDX_ROOT=/r"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}

	// sandbox on: workspace-write, the shared .git and allow_write writable, network on
	on := true
	a.Sandbox = config.Sandbox{Enabled: &on, AllowWrite: []string{"/caches/go-build"}}
	cmd, err = ad.Command(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	argv = strings.Join(cmd.Args, " ")
	common, _ := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	var dirs []string
	for i, s := range cmd.Args {
		if s == "--add-dir" {
			d, _ := filepath.EvalSymlinks(cmd.Args[i+1])
			if d == "" {
				d = cmd.Args[i+1]
			}
			dirs = append(dirs, d)
		}
	}
	if !strings.Contains(argv, "--sandbox workspace-write") || !strings.Contains(argv, "network_access=true") ||
		!slices.Contains(dirs, common) || !slices.Contains(dirs, "/caches/go-build") {
		t.Fatalf("sandbox on: argv %s\nwritable %v, want the shared .git %s and the cache", argv, dirs, common)
	}

	a.Strict = true
	if _, err := ad.Command(context.Background(), a); err == nil || !strings.Contains(err.Error(), "strict") {
		t.Fatalf("strict: err = %v, want a refusal", err)
	}
}

func TestCodexTwoServersOneEnvNameIsRefused(t *testing.T) {
	_, wt := worktreeOf(t)
	a := Attempt{Worktree: wt, Dir: t.TempDir(), SwarmBin: "/opt/swarm", ExtraMCP: map[string]config.MCPServer{
		"a": {Command: "x", Env: map[string]string{"KEY": "1"}}, "b": {Command: "y", Env: map[string]string{"KEY": "2"}}}}
	ad, _ := New("c", config.Provider{Kind: "codex"}, config.Profile{})
	if _, err := ad.Command(context.Background(), a); err == nil || !strings.Contains(err.Error(), "both set KEY") {
		t.Fatalf("err = %v, want a refusal naming KEY", err)
	}
}

func TestParseResultReadsCodexEvents(t *testing.T) {
	done := `{"type":"thread.started","thread_id":"x"}
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":800,"output_tokens":300}}`
	if r := ParseResult(done); !r.Known || r.TokensIn != 1200 || r.TokensOut != 300 || r.LimitedUntil != 0 {
		t.Fatalf("completed turn: %+v", r)
	}
	limited := `{"type":"thread.started","thread_id":"x"}
{"type":"error","message":"You've hit your usage limit. Upgrade to Pro or try again later."}
{"type":"turn.failed","error":{"message":"You've hit your usage limit. Upgrade to Pro or try again later."}}`
	r := ParseResult(limited)
	if !r.Known || r.LimitedUntil < time.Now().Add(4*time.Hour).Unix() {
		t.Fatalf("usage limit: %+v, want the account cooled for hours", r)
	}
	weekly := `{"type":"turn.failed","error":{"message":"You've hit your usage limit. Upgrade to Pro, or try again in 2 days 3 hours 10 minutes."}}`
	want := time.Now().Add(51*time.Hour + 10*time.Minute).Unix()
	if r := ParseResult(weekly); r.LimitedUntil < want-60 || r.LimitedUntil > want+60 {
		t.Fatalf("weekly limit cooled until %d, want about %d (in 2 days 3 hours 10 minutes)", r.LimitedUntil, want)
	}
	// the real words, codex-cli 0.147 with an empty CODEX_HOME
	notLoggedIn := `{"type":"thread.started","thread_id":"x"}
{"type":"error","message":"Reconnecting... 5/5 (unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: https://api.openai.com/v1/responses)"}
{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: https://api.openai.com/v1/responses"}}`
	if r := ParseResult(notLoggedIn); !r.AuthFailed {
		t.Fatalf("login failure not seen: %+v", r)
	}
	// a worker that greps a file mentioning codex login must not take its provider down
	if r := ParseResult(`{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"run codex login"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`); r.AuthFailed {
		t.Fatalf("output of a command read as a login failure: %+v", r)
	}
}
