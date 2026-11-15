# Hardening pass (2026-11-15)

From the /simplify review and the architecture questions (state machine, graphs, events,
DDD, schemas, ORM). One commit per step, tests first where a contract exists.

| # | Step | Why | Status |
|---|---|---|---|
| 1 | Schema migrations via `PRAGMA user_version`; drop stored `branch`/`worktree` (derived from repo + id) | an old db must upgrade, not silently miss columns; derived state cannot disagree with git | done |
| 2 | Explicit state machine: `Status` type, transition table, `Move(id, from, to, Patch)` guarded by `WHERE status IN (from)`; status sets (terminal, active, waiting) defined once | ~15 stringly `Set(map)` calls with no status guard: a `report_done` landing between a read and a failure write was overwritten | done |
| 3 | Restart reconcile: running tasks of this node with no live worker go back to ready at start, not after the lease, and not counted as infra failures | a restart cost every running task 2 min and one of its 3 infra strikes | done |
| 4 | Merge failure moves the task to blocked with the reason instead of retrying the same ff every 2 s and holding the queue | one dirty main checkout froze every merge behind it | done |
| 5 | Gate concurrency cap (`limits.max_gates`, default half of max_workers) | 10 suites at once thrash the machine and time out honest work as gate_fail | done |
| 6 | Worker start off the tick: claim on the loop, worktree + launch in a goroutine, result back to the loop | 10 ready tasks = 10 serial `git worktree add` inside one tick | done |
| 7 | Board: one handler table in the service; RPC and MCP generated from it | each operation was written three times (service, RPC, MCP) | done |
| 8 | `swarm gate` for workers (exact same check as the daemon); permission derivation skips shells and env prefixes | `Bash(sh:*)` / `Bash(cd:*)` from gate commands were too wide or useless | done |
| 9 | Dependencies through the session tool (`needs`), drafts promoted when deps merge; cycles impossible by construction (edges only from a new task to existing ones) | the DAG existed in the schema but nothing could create an edge | done |
| 10 | `swarm schema` prints the JSON Schema of swarm.toml; test validates the shipped example against it | editor completion and validation; a schema that drifts from the code fails the test | done |
| 11 | sqlc for static queries (typed Go from SQL) | decided at step 11 after 2 removes the stringly part; see outcome | deferred |

## Outcome

Steps 1-10 landed, each with a test that goes red without the change where one could be
written (the restart test was run against a disabled reconcile: it timed out at 61 s).

**Step 11, sqlc: deferred, with a trigger.** Measured: 22 queries, 2 of them dynamic
(`IN` over a status list) that sqlc would not generate. Step 2 already removed the stringly
part (`Set(map)` became `Move` + typed `Patch`). sqlc would add a generator step and a
second style of data access for about ten typed scans. What was worth taking now was done
instead: the four queries living outside `internal/db` moved into it, so all SQL is in one
package. Revisit when the query count passes ~40, or at the first bug from a column/scan
mismatch.

**Answers to the architecture questions, as built:**
- State machine: explicit (`internal/db/state.go`), transitions in one table, guarded moves.
- Graph: tasks form a DAG through `deps`; acyclic by construction.
- Events: a reconcile loop (level-triggered, like a controller) woken early by signals;
  `events` is the audit log, including every status move. No event sourcing.
- DDD: bounded contexts are the packages (dispatch, board, gate, provider, db); the task
  with its attempts is the aggregate; no repository/factory ceremony.
- Schemas: SQL schema with versioned migrations; MCP input schemas and the swarm.toml
  JSON Schema are inferred from Go types and pinned by tests.
- ORM: none; database/sql with all SQL in `internal/db`.

# Pass 2: what the PRD asks that was missed, and the registry (2026-11-15)

Found by reading the PRD section by section against the code. One was a shipped bug:

- **Fixed (6600f0e):** every task was measured against the run's base. After a rebase its
  diff held other tasks' merged files and the gate failed it for touching files it does not
  own; a task started after its dependency merged got a worktree without the dependency.
  Reproduced with a two-task test before fixing.

## Where the registry lives

| What | Where | Why there |
|---|---|---|
| Subscriptions (accounts): kind, family, slots, tiers, models, **roles**, **priority**, **daily budget** | `swarm.toml` of the profile, `[providers.<id>]` | declared, reviewed as a diff; the profile is the world, so solo and Epiphan never share an account by construction |
| Machines: which accounts are logged in there, max workers, roles | `swarm.toml`, `[nodes.<id>]` | same |
| Live state of each subscription: ok / cooling / disabled, until when, why | profile db, table `providers` | changes by the minute; only swarmd writes it |
| Spend: tokens and dollars per attempt | profile db, `attempts` | measured from the CLI's final event |
| Rate-limit windows of a Claude account (5 h, week) | the harness's `out/limits/*.json` (status line) | already measured there; swarm reads, never duplicates (plan phase 3) |
| Workers | not a registry: an attempt is a process with a row in `attempts` | a worker lives one attempt; `swarm providers` shows who is busy |

Roles: `worker` (does tasks), `reviewer` (reviews another family's diff, phase 4), `lead`
(decomposes, phase 2: Fable only, by the PRD). A provider without `roles` is a worker.
Priority: lower first among providers that can take a tier; ties by name. Task priority
already orders the queue.

| # | Step | PRD | Status |
|---|---|---|---|
| 12 | Never run two tasks whose write claims overlap (path or `dir/` prefix) at once | §7 Schedule, MUST | done |
| 13 | Single writer: `swarm task add` goes through the daemon when it is running | §3, §5 MUST | done |
| 14 | Worker env: `DISABLE_AUTOUPDATER=1`, a slot number, `PORT=20000+slot*10` | §8, §12 | done |
| 15 | Provider registry: `roles`, `priority`, `budget_usd_day` (spent → cooling to next UTC midnight); live state in `providers`; `swarm providers` and in `swarm_status` | §2, §7, §11 | done |
| 16 | Notify on run done and on a task blocked; `caffeinate` on macOS while running; hourly `git worktree prune` | §11, §12, §8 | |
| 17 | A failed gate command is rerun once before the verdict (flaky tests) | §12 | |
| 18 | Commit trailer `Swarm-Provider` in the worker prompt | §8 | done |

Still open, by phase (not in this pass): the lead and its tools (`create_tasks`, `resolve`,
`update_task`, `announce`, `split_task`, `claim_files`, `list_notes`, wake triggers, final
integration task); codex adapter, cross-family review, 429/quota patterns from config,
cooling with backoff and a probe run, escalation to a higher tier on attempt 3, overflow to
`claude-api` with a $ cap; containers and egress allowlist; `swarm report`, `logs`, `stop`,
`pause`, log retention; the harness-core contract (runs.jsonl, `task item --done`, limits);
the solo and Epiphan profiles with `merge = "pr"`.
