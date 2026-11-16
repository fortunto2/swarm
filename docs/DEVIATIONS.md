# Deviations from prd.md

Each line: what the PRD says, what was done, why.

- §5 schema: added `tasks.repo`, `tasks.node`, `attempts.node`, `runs.topic`, table `nodes`. One profile spans several repos and nodes (unification.md §2, §4).
- §5 path: db is `<profile>/state/swarm.db`, not `<repo>/.swarm/swarm.db`. Usage is accounted per world, not per repo.
- §8 worktrees: under `profile.worktree_root`, never inside a repo; config load refuses a root inside any repo (a worktree inside a repo was once swept into a commit by `git add -A`).
- §2 secrets in swarm.toml: a token-looking env key must hold `secret:<name>` or `env:<VAR>`; a literal is refused at load.
- §4 module path is `github.com/fortunto2/swarm`.
- §6 worker shell access: besides MCP, `swarm board done|blocked|task` talks to the board from a worker's shell (env SWARM_SOCKET, SWARM_TOKEN). Scripted workers and debugging need it; identity is the same token.
- §5 socket: `/tmp/swarm-<uid>/<hash(state_dir)>.sock`, not `<repo>/.swarm/swarmd.sock`: a unix socket path is capped near 104 bytes and profile paths exceed it.
- §7 infra failures: a task the infrastructure fails 3 times goes to `blocked` instead of cycling forever (PRD sets no cap).
- §9 review: a stage of the gate (`internal/review`), not a reviewer attempt scheduled on its own: it runs where the gate runs (a node included), on that machine's account named by `review`, and its rejection is a gate failure the next attempt reads. The `review` status stays unused. Family crossing is the operator's choice of account, not enforced: on the first day the second readers that found real bugs were the same family as the authors.
- extra: `[mcp.<name>]` extra MCP servers for agents and `[hooks] after_merge`, so a code/session index (solograph) can serve workers and be refreshed after every merge, without the core knowing its name.
- §8 permissions without a sandbox: `--allowedTools` derived from the board, extra MCP servers, git on the own branch (no push) and the first word of each gate command; `allowed_tools` in the provider adds more.
- §8 worker settings: the operator's user-level hooks/skills load in workers by default (they help); `setting_sources` narrows it per provider.
- §6 session tools: the lead's tools exist first as session tools for any Claude Code session (status, add, list, notes) under an operator token; the model lead (phase 3) will use the same door.
- §5 task life cycle: an explicit transition table and guarded `Move` (the PRD lists the statuses; it does not say who may move what). `review` is reachable but unused until phase 4.
- §5 storage: `branch` and `worktree` are not stored; both derive from (repo, id). Schema versioned with `PRAGMA user_version`.
- §7 restart: running tasks are reconciled at daemon start instead of waiting for lease expiry, and the restart counts as no failure.
- §9 merge: a failed fast-forward blocks the task with the reason; the PRD only covers rebase conflicts.
- §8 gate for workers: `swarm gate` instead of listing the gate commands in the prompt; permission rules from gate commands skip shells, cd, env.
- sqlc (Schemas First) deferred with a trigger; see docs/hardening.md.
- §2 registry: roles, priority and a daily dollar budget per provider in swarm.toml; live state in `providers`. The PRD's fixed tier orders (B: kimi → claude-b → codex) are expressed as priorities, not code.
- §7 tasks without file claims are not checked for overlap: declaring files is what buys the guarantee.
- §12 flaky tests: a failed gate command is rerun once (`gate_reruns`, default 1) and named in a notification; quarantine is left to a person.
- §10 security: two levels instead of one. `trusted` (default) runs workers without a container, as the PRD allows for supervised runs; `strict` uses Claude Code's native OS sandbox instead of Docker for the worker. The gate's own sandbox and per-task caches are not built yet (docs/hardening.md).

## Nodes: one SQLite coordinator, machines dial in (2026-11-15)

PRD phase 5 said "Postgres instead of SQLite when workers move to a VPS". Built instead: the
database stays one SQLite file with one writer (the coordinator), and other machines join as
nodes that dial in over a command's stdin/stdout (ssh in practice) and take orders through
three board operations. Why: a shared database would give every machine a writer and undo
the PRD's own rule that only swarmd writes; a laptop behind NAT cannot be dialled, so the
node must call out; and ssh already authenticates the link. Cost: the coordinator is the one
place that must be up (put it on an always-on machine). docs/nodes.md.
