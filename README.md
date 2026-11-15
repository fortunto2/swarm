# swarm

One lead, many subscriptions, merged code. `swarm` runs coding agents on the official
CLIs you already pay for (Claude Code, Codex), each task in its own git worktree, and lets
nothing into `main` that has not passed your tests.

Status: **early**. Phase 1 works end to end on a scripted worker: a task goes
`ready → running → verifying → merged` through the board, the gate and a fast-forward
merge, and a worker that deletes or skips a test, touches files it does not own, or dies
silently never reaches `main`. The lead, several providers and the sandbox are next
(see *Roadmap*).

## Why

A single agent session is limited by one context and one rate limit. Ten sessions on one
repository step on each other's files, report "done" without running tests, and burn a
week of quota by Wednesday. `swarm` is the dull part that makes many agents usable:

- **Deterministic dispatcher, no model in the loop.** It claims tasks atomically, launches
  workers, reaps the dead, retries, merges. Models only work on tasks.
- **The gate decides, not the agent.** `report_done` hands a commit to the gate: static
  checks (files inside the task's claims, no deleted or skipped tests), then your own test
  command, then a fast-forward into `main`. Rebased and re-gated when `main` moved.
- **One door.** Agents reach the system only through a small MCP server (`board`), with a
  one-time token per attempt, so a worker cannot speak as another task.
- **Official CLIs only.** No proxies that wrap a subscription's OAuth into an API, no
  third-party harnesses with a Claude subscription login.
- **Profiles, not forks.** Everything that differs between your worlds (providers, repos,
  merge policy, secrets, extra tools) lives in a profile's `swarm.toml`. The core holds no
  names.

## Quick start

Full guide, written so an agent can follow it in a fresh session: [`docs/setup.md`](docs/setup.md)
(install, a profile, the daemon under launchd, MCP for sessions, `swarm selftest`, the first task).

```sh
go install github.com/fortunto2/swarm/cmd/swarm@latest

mkdir -p ~/swarm-profiles/me && cp configs/swarm.example.toml ~/swarm-profiles/me/swarm.toml
# edit repos.*.path, gate, providers; then:
export SWARM_PROFILE=~/swarm-profiles/me
swarm check            # validates the profile, lists every problem at once
swarm init             # creates the profile's SQLite
swarm selftest         # one task through a real provider, gate and ff merge, on a throwaway repo

swarm task add --repo app --title "add Two" \
  --spec "add func Two() int returning 2, with a test" \
  --acceptance "go test passes; TestTwo exists" --files two.go,two_test.go
swarm daemon --until-idle
swarm status
```

Try it without spending quota: a provider with `kind = "mock"` runs a script instead of a
model, over the same board protocol (`cmd/swarm/e2e_test.go` has working scripts).

## Safety model

Two levels, set per profile with `safety`.

**`trusted` (default)**: your own workers, a small team. Guards against mistakes, at no
cost to the work:

- the gate decides, not the worker: claimed files only (a task without `--files` takes the
  whole repo and runs alone), no deleted, skipped or focused tests, the repo's own checks;
- what merges is the gated commit, as a fast-forward; nothing is ever pushed;
- Claude's tools cannot read the profile's state (operator token), account dirs or key
  stores; the daemon's git runs no hooks or filters from the repo; gate commands get no
  account or billing variables;
- changes to agent, CI or git configuration and test files that shrank are reported as
  warnings on the task, not failures.

**`strict`**: workers treated as untrusted (outside contributors, unattended overnight
runs on someone else's code). Adds Claude Code's OS sandbox around every worker's Bash and
the processes it starts (no reads of credential paths, writes only to the worktree and
caches, network only to `[sandbox] allowed_domains`), user-level settings only, an
environment allowlist, explicit tool lists for extra MCP servers, files required on every
task, and protected paths and shrinking tests fail the gate. The sandbox can also be
switched on alone with `[sandbox] enabled = true`.

**Secrets** stay in the OS keystore. `secret_cmd` and `secret_exec` in the profile point at
a keystore CLI: a provider key written as `secret:<name>` becomes Claude Code's
`apiKeyHelper` (asked at request time, never in env or a file), and an MCP server's
secrets reach its own process through `secret_exec`, so its config file names them only.
Text swarm stores or forwards outside Claude (gate output, worker notes and questions,
notifications) goes through the `redact` filter first; if the filter fails, the text is
withheld rather than forwarded.

Blocked tasks wait for a person: `swarm task retry <id>` (counters reset) or
`swarm task cancel <id>` (a running worker is stopped), or the same through MCP.

## Dependencies

A task can wait for others: `swarm task add --needs 3,4` (or `needs` in `swarm_add_task`).
It sits as `draft` until every task it needs is merged, then becomes `ready` and is built on
top of them. Edges only go from a new task to existing ones, so the graph has no cycles.

## How a task moves

```
ready ──claim──▶ running ──report_done──▶ verifying ──gate ok──▶ merging ──ff──▶ merged
  ▲                 │                         │                      │
  │  died silently  │   gate failed           │   main moved         │
  └─(infra, ≤3)─────┘◀──(attempt, ≤max)───────┘◀──rebase, re-gate────┘
                    blocked: out of attempts, or report_blocked → the lead (phase 2) or you
```

A failure that is the model's (gate, timeout) counts as an attempt and its log tail goes
into the next attempt's prompt. A failure that is the infrastructure's (crash, lost lease)
does not count against the model. A daemon restart puts its running tasks back at once and
counts nothing against them. A merge git refuses blocks that one task with the reason.

Every status change goes through one transition table (`internal/db/state.go`) and is
guarded in the same UPDATE by the status the writer expected, so a late failure cannot
overwrite a submission that arrived first. Workers run `swarm gate` before submitting: the
same static checks and gate commands the daemon will run.

## Profile

`configs/swarm.example.toml` is the full reference. The parts that matter:

| Section | What |
|---|---|
| `[profile]` | name, `worktree_root` (must be outside every repo), `state_dir`, `notify`, `secret_cmd` |
| `[repos.<name>]` | path, `gate` commands, `merge = "ff"` (or `"pr"`, coming) |
| `[providers.<id>]` | `kind` (claude, codex, mock), `family` (review crosses families), slots, tiers, models, env |
| `[nodes.<id>]` | where workers run: `local` or `ssh:<alias>` (multi-node is on the roadmap) |
| `[mcp.<name>]` | extra read-only MCP servers for workers and the lead |
| `[hooks]` | `after_merge` commands, e.g. reindex the repo |

`swarm schema` prints the JSON Schema of the file (also in `configs/swarm.schema.json`);
the example starts with `#:schema ./swarm.schema.json`, which TOML editors use for
completion and validation.

### The provider registry

Each `[providers.<id>]` is one subscription. Declared there: kind, family, slots, tiers,
`roles` (worker, reviewer, lead), `priority` (lower first for a tier) and `budget_usd_day`.
Live in the profile's database: whether it is ok, cooling until a time, or disabled, and why;
spend comes from each attempt's own cost report. A provider over its daily budget pauses
until the next UTC midnight; one that cannot log in is disabled until the daemon restarts.

```
$ swarm providers
PROVIDER       KIND    FAMILY     ROLES                  PRIO   SLOTS STATE     TODAY
kimi           claude  moonshot   worker                    1   2/4   ok        $3.10 of $20.00
codex          codex   openai     worker,reviewer           2   1/3   ok        $0.00 no cap
claude-a       claude  anthropic  lead,worker,reviewer      3   0/3   cooling until 2026-11-16T00:00:00Z $41.20 of $40.00
```

Workers are not a registry of their own: a worker lives one attempt, a row in `attempts`.
Machines are `[nodes.<id>]`: which accounts are logged in there and how many workers fit.

Secrets are never values in the file: a token-looking key must hold `secret:<name>`
(resolved by `secret_cmd`) or `env:<VAR>`, and `swarm check` refuses anything else.

### Code search for workers

Workers find their way faster with an index of the code and of past sessions.
[solograph](https://github.com/fortunto2/solograph) is one binary that serves both over MCP;
add it as `[mcp.solograph]` and reindex after each merge with an `after_merge` hook, as the
example config does. Any MCP server works the same way.

## Roadmap

1. ~~Skeleton: config, schema, atomic claim~~
2. ~~One provider, one worker: board, worktree, gate, ff merge~~ (scripted and live Claude runs)
   ~~Session tools: any Claude Code session queues and watches work over MCP~~
3. Lead and task tree: decomposition, `split_task`, dependencies, file claims, batched merge queue
4. Several providers: Codex adapter, tiers, cross-family review, rate-limit detection and failover
5. Sandbox: workers in containers with an egress allowlist; unattended overnight runs
6. `merge = "pr"`: leave a branch and a PR description for a human to push
7. Nodes: workers on other machines over SSH, one coordinator per profile
8. Scheduled and event work (`tick`, `event`, `script`) on the same queue

Deviations from the original design are logged in `docs/DEVIATIONS.md`.

## From any Claude Code session, its subagents and teams

Register the session side once:

```sh
claude mcp add swarm -- swarm mcp --profile ~/swarm-profiles/me
```

Every session then has `swarm_status`, `swarm_add_task`, `swarm_list_tasks` and
`swarm_task_notes`, and so do its subagents and agent-team teammates, which inherit the
session's MCP servers. A planning session queues the work, the daemon's workers do it,
and anyone can watch it reach `merged`. Sessions queue and watch; only the gate decides
that something is done. The session authenticates with an operator token kept in the
profile's state dir (mode 0600); a worker's per-attempt token cannot reach these tools.

## Budget

Workers keep your hooks, skills and plugins: they help. What swarm watches instead is
spend. Each attempt records tokens and dollars from the CLI's own final event (NULL when
the CLI does not say). On a subscription those dollars are the CLI's list-price estimate
of the quota used, not money billed; `budget_usd_day` caps that estimate. `swarm_status` shows the last 24 hours, and per-provider limits
and failover arrive with several providers. A CLI that is not logged in takes its
provider out of rotation on the first try instead of burning retries.

## Using it from Claude Code

`skills/swarm/SKILL.md` teaches an agent to drive `swarm`: write a task that a gate can
check, size it, add it, run the daemon, read why something is blocked. Install the repo as
a plugin, or symlink the skill into `~/.claude/skills/`.

## License

MIT
