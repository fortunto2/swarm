# CLAUDE.md: working on swarm itself

A Go daemon + CLI that runs coding agents (official CLIs, under subscriptions) on tasks,
each in its own git worktree, and merges only what passes the repo's own gate.
Setting it up for yourself: [`docs/setup.md`](docs/setup.md). Using it from a session:
[`skills/swarm/SKILL.md`](skills/swarm/SKILL.md). Why things are the way they are:
[`docs/hardening.md`](docs/hardening.md), [`docs/DEVIATIONS.md`](docs/DEVIATIONS.md).

## Layout

| Package | What |
|---|---|
| `cmd/swarm` | CLI (cobra): check, init, status, providers, report, schema, task add/retry/cancel, daemon, mcp, board, gate. `e2e_test.go` drives the real binary with scripted (mock) workers |
| `internal/config` | `swarm.toml` load, validation (every problem at once), JSON Schema (`make schema`) |
| `internal/db` | SQLite, the only writer. `state.go` is the task state machine: every status change goes through `Move`, guarded by the expected status. Migrations are append-only |
| `internal/dispatch` | the daemon loop: reap, promote, schedule, gate, merge; provider registry; hooks; reconcile after a restart |
| `internal/board` | the one door for agents: an op table (role, input type, handler) served over a unix socket and as MCP |
| `internal/gate` | static checks (claims, tests deleted/skipped/shrunk, protected paths) and the repo's gate commands |
| `internal/provider` | turns an attempt into a `claude` process: allowed tools, deny rules, OS sandbox (strict), env, MCP config |
| `internal/envx`, `gitx`, `redact`, `worktree`, `prompts` | small helpers |

## Rules for changes

- **Public repo.** No client names, private paths, account ids or personal details in
  code, docs or tests. Examples use `example.com`. Scan before every push:
  `grep -rniE "<your private words>" $(git ls-files)`.
- **The core knows no names.** Anything that differs between users (providers, repos,
  boards, task registries, notifiers) is profile config or a hook, never code here.
- **Statuses change only through `db.Move`.** A new transition goes into the table in
  `state.go` and into `TestTransitionTableIsPinned`.
- **A behaviour without a test that goes red when it is removed is not done.** Write the
  test, break the code on purpose once, see it fail, restore. Several tests here were
  first written so they passed without the feature; that is the failure to look for.
- **The daemon loop never blocks**: slow work (worktree checkout, gates, hooks, notify)
  runs in goroutines and comes back through channels; only the loop touches its maps.
- **Measure, then claim.** Behaviour of the `claude` CLI (permissions, sandbox, settings)
  is checked live with a known-answer task before docs state it.

## Commands

```sh
make test        # go test ./... -count=1 (e2e builds the binary, ~70 s)
go test ./... -race -count=1
make schema      # regenerate configs/swarm.schema.json after a config change
make build       # bin/swarm
```
