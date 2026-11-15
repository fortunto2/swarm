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

## 5. Prove the loop: `swarm selftest`

```sh
swarm --profile <dir> selftest                       # the first enabled worker by priority
swarm --profile <dir> selftest --provider claude-me  # a given one; also --timeout 10m, --keep
```

It runs one task (`answer.txt` holds 42) through a real provider, the gate and an ff merge,
on a throwaway git repo in a temp dir, and removes the dir after. None of your repos is
touched, and it works next to the profile's own daemon: the temp profile has its own state,
database and lock. It carries the chosen provider, your nodes, safety, sandbox and secret
helpers (secrets as `secret:`/`env:` references, never values); hooks, `notify` and extra
MCP servers stay out. The worker gets one attempt, on the cheapest tier the provider takes.
It runs with your shell's environment, not launchd's: a PATH only the daemon lacks still
shows up in its first real task.

Exit 0 and one line on a pass (the dollars are the CLI's list-price figure, see Budget in
the README), exit 1 and a reason otherwise:

```
selftest ok: <provider> merged in <duration>, $<cost>
selftest FAILED: blocked: attempt 1: gate_fail; gate: test "$(cat answer.txt)" = 42: exit 1
```

A FAILED line is `<status>: <reason>`, the reason being what swarm recorded:

| Line says | Means |
|---|---|
| `setup: no provider to test: …` | nothing ran: no enabled worker this node may run; the reason per provider follows |
| `ready: provider X disabled: not logged in` | that provider's CLI has no login: `CLAUDE_CONFIG_DIR=<its config_dir> claude`, then `/login` |
| `blocked: crash: worker exited without report_done …` | the worker died three times without a word; rerun with `--keep` and read `state/attempts/*/log.jsonl` |
| `blocked: crash: exec: "claude": executable file not found in $PATH` | the CLI is not on this shell's PATH |
| `blocked: attempt 1: gate_fail; …` | the worker ran and submitted, but its answer failed the gate: look at the model, its tier, the prompt |
| `blocked: the model refused this task: …` | the provider's safeguards stopped it |
| `running: no result within 10m0s (--timeout)` | the loop had not finished when the clock ran out |

`--keep` leaves the dir (repo, `swarm.toml`, `state/`, `daemon.log`) and prints its path.

Then a real task in your own repo:

```sh
swarm --profile <dir> task add --repo app --title "add a tiny function" \
  --files pkg/tiny.go,pkg/tiny_test.go \
  --spec "add func Tiny() int returning 1 in pkg/tiny.go, with a test" \
  --acceptance "the repo's gate passes and the test exists"
swarm --profile <dir> status          # ready -> running -> verifying -> merging -> merged
```

If it blocks: `swarm --profile <dir> task notes <id>` (and `report` for the week) says why (often the task:
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
- `[limits] admit`: a shell command asked before new workers or gates start, for a host that
  also runs other work. Exit 0 admits; anything else holds, and its first output line is
  logged (`held`, with the reason) once per change. Hold while another job runs, or while
  memory is low: a type check on a big TypeScript repo can take 3 GB, and a gate that dies
  of it is an attempt spent on nothing in the code.
- `swarm report --since 7d`: what a week says; a weekly look at it is how the setup gets better.
