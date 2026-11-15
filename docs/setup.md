# Setting swarm up for yourself

Written so an agent (Claude Code, Codex) can follow it top to bottom in a fresh session.
About 10 minutes. Every step says how to check it worked.

## 0. What you get

A daemon (`swarmd`) per *profile*. A profile is one world: its repos, its subscriptions,
its database. Two worlds (say, personal and work) are two profiles that share nothing.
Any Claude Code session of that world queues and watches work through MCP tools; workers
run on the official `claude` CLI under your subscription, each in its own git worktree,
and only a commit that passed the repo's own tests reaches `main`.

## 1. Install

```sh
go install github.com/fortunto2/swarm/cmd/swarm@latest   # Go 1.23+
ln -sf "$(go env GOPATH)/bin/swarm" ~/.local/bin/swarm     # if GOPATH/bin is not on PATH
swarm --help
```

Also needed: `git`, the `claude` CLI logged in, and whatever your repos' tests need
(`go`, `uv`, `npm`...). `sqlite3` is optional (handy for reading notes).

## 2. A profile

Pick a directory you keep (a private repo is a good home; never inside a repo swarm will
write to). Put `swarm.toml` there, starting from
[`configs/swarm.example.toml`](../configs/swarm.example.toml):

```toml
#:schema https://raw.githubusercontent.com/fortunto2/swarm/main/configs/swarm.schema.json
[profile]
name = "me"
state_dir = "state"                 # db, logs, operator token; gitignore it
worktree_root = "~/.swarm/wt/me"    # outside every repo
safety = "trusted"                  # your own workers; "strict" = OS sandbox and more

[repos.app]
path = "~/code/app"
main_branch = "main"
gate = ["go test ./... -count=1"]   # what decides a task is done: the repo's own checks
merge = "ff"                        # main moves locally; pushing stays yours

[limits]
max_workers = 3

[providers.claude-me]
kind = "claude"
family = "anthropic"
slots = 3
tiers = ["A", "B"]
models = { A = "opus", B = "sonnet" }
# config_dir = "~/.claude-work"     # only for a second Claude config; omit for the default login
```

Check: `swarm --profile <dir> check` prints `ok: profile ...`, then
`swarm --profile <dir> init` creates the database. Gitignore `<dir>/state/`.

Two things people get wrong:

- **One profile per Claude config.** If a world uses its own `CLAUDE_CONFIG_DIR`, set the
  provider's `config_dir` to it; with the default login leave it out. A worker never
  inherits the daemon's `CLAUDE_CONFIG_DIR`, `ANTHROPIC_*` or OAuth token.
- **Secrets are references, never values**: `secret:<name>` (resolved by `secret_cmd`, e.g.
  a keystore CLI) or `env:<VAR>`. `swarm check` refuses a literal token.

## 3. The daemon, always on (macOS launchd)

`~/Library/LaunchAgents/<label>.plist`, with absolute paths:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.me.swarmd</string>
  <key>ProgramArguments</key><array>
    <string>/Users/me/go/bin/swarm</string><string>--profile</string><string>/Users/me/profile</string><string>daemon</string>
  </array>
  <key>EnvironmentVariables</key><dict>
    <key>PATH</key><string>/Users/me/.local/bin:/Users/me/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/me/profile/state/swarmd.log</string>
  <key>StandardErrorPath</key><string>/Users/me/profile/state/swarmd.log</string>
</dict></plist>
```

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.me.swarmd.plist
launchctl list | grep swarmd           # a pid means it runs
```

**PATH is the trap.** launchd does not read your shell profile. Put on PATH every tool your
gate and your hooks call, *the same versions your shell uses*: a hook calling a Python
script that imports a library will fail silently under another `python3`. Test a hook
command with `env -i HOME=$HOME PATH=<that PATH> <command>` before trusting it.

Without launchd: `swarm --profile <dir> daemon` in a terminal, or `--until-idle` to run what
is queued and exit. One daemon per profile; a second refuses.

## 4. Sessions see the swarm (MCP)

Register once per Claude config, pointing at that config's profile:

```sh
claude mcp add swarm -s user -- /Users/me/go/bin/swarm mcp --profile /Users/me/profile
# a second config: CLAUDE_CONFIG_DIR=~/.claude-work claude mcp add swarm -s user -- ... --profile <work profile>
```

New sessions get `swarm_status`, `swarm_add_task`, `swarm_list_tasks`, `swarm_task_notes`,
`swarm_retry_task`, `swarm_cancel_task`; subagents and agent-team teammates inherit them.
The server starts even while the daemon is down and answers `daemon_down` then.

Optional: the skill in [`skills/swarm/SKILL.md`](../skills/swarm/SKILL.md) teaches an agent
how to write tasks a gate can judge. Symlink it into `~/.claude/skills/swarm`.

## 5. First task: prove the loop

```sh
swarm --profile <dir> task add --repo app --title "add a tiny function" \
  --files pkg/tiny.go,pkg/tiny_test.go \
  --spec "add func Tiny() int returning 1 in pkg/tiny.go, with a test" \
  --acceptance "the repo's gate passes and the test exists"
swarm --profile <dir> status          # ready -> running -> verifying -> merging -> merged
git -C ~/code/app log -1 --format='%an %s'   # author swarm-<provider>-T1-1
```

If it blocks: `swarm --profile <dir> report` and the task's notes say why (often the task:
no files named, an acceptance the gate cannot see, a file it needs not yet committed).
`swarm task retry <id>` / `swarm task cancel <id>` (daemon running).

## 6. Optional wiring

- `redact`: a filter over text swarm stores or forwards (gate output, worker notes,
  notifications), e.g. `secret_guard.py filter` from solo-factory.
- `notify`: a command that gets one message per event that needs you (blocked, run done).
- `[hooks] on_status`: a command per status change (`{task} {title} {status} {worktree}
  {repo} {topic} {commit}`): mirror tasks on a board (Orca: `orca worktree set --worktree
  path:{worktree} --workspace-status in-progress`), or note them on your own task registry.
- `[hooks] after_merge`: e.g. reindex the repo for a code-search MCP.
- `[mcp.<name>]`: extra MCP servers for workers (`tools` lists what they may call).
- `swarm report --since 7d`: what a week says; a weekly look at it is how the setup gets better.
