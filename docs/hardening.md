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
| 10 | `swarm schema` prints the JSON Schema of swarm.toml; test validates the shipped example against it | editor completion and validation; a schema that drifts from the code fails the test | |
| 11 | sqlc for static queries (typed Go from SQL) | decided at step 11 after 2 removes the stringly part; see outcome | |

## Outcome

(filled in as steps land)
