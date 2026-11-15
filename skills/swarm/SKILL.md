---
name: swarm
description: Hand coding work to a swarm of agents that run on official CLIs in separate git worktrees, with a test gate before anything reaches main. Use when the user says "swarm", "раздай агентам", "запусти воркеров", "run this in parallel on several agents", has a spec too big for one session, or asks why a swarm task is blocked. Only for work a repo's tests can judge, usually a batch of tasks; the default is to do it yourself. Not for a one-file change (do it yourself) and not for analysis fan-outs that need no worktree (use a Claude workflow).
---

# swarm

`swarm` is a CLI plus a daemon. You do not do the tasks; you write them so a gate can
judge them, start the daemon, and read the result. The profile (`SWARM_PROFILE` or
`--profile`) says which repos, providers and policies apply. Never guess a profile: if
neither is set, ask which one.

## When it pays, and when it does not

Default: do the work yourself. Reach for swarm only when the user asks for it, or when
there is a queue of independent tasks that a gate can judge and the session has other
work to do meanwhile.

It pays when all of these hold:
- the repo has tests that would go red without the change, so the gate means something;
- the acceptance is checkable by those tests, not by taste;
- the task is 15+ minutes of work, or there are several that can run in parallel.

It does not pay for:
- a one-file or one-function change: worker start-up, gate and merge cost minutes and
  quota that an inline edit does not;
- work where deciding what to do is the hard part: writing the spec *is* the work;
- a repo without meaningful tests: the gate passes anything;
- analysis or exploration with no commit at the end (use a workflow or a subagent).

Measured on a live run: a ~700-line feature with tests took ~15 minutes of worker time
and about $4 at list price on the top tier; a one-function task, $0.19. Restarting the
daemon while tasks run kills their workers and the work is redone: install the new binary and
run `swarm drain` instead (finishes what runs, starts nothing new, exits for launchd or
systemd to start the new one; on a node profile it drains the node). Until
then `swarm status` says `daemon runs <commit>, this binary is <commit>`; `swarm version`
prints the commit a binary was built from.

## 0. Two ways in

- **MCP (preferred inside a session):** tools `swarm_status`, `swarm_add_task`,
  `swarm_list_tasks`, `swarm_task_notes`, after `claude mcp add swarm -- swarm mcp --profile <dir>`.
  Subagents and teammates have them too, so a planning fan-out can queue tasks directly.
  They need a running `swarm daemon` for the profile.
- **CLI:** the commands below. Same rules either way.

## 1. Before adding anything

```sh
swarm check     # the profile is valid; prints repos, providers, nodes
swarm status    # what is already queued; do not add a duplicate
```

## 2. Write tasks a gate can check

Each task is 15–60 minutes of work. Smaller ones waste a worker's start-up; bigger ones
fail the wall clock. For each:

- `--title`: one line.
- `--spec`: what to change and where. Name the files and functions, and the pattern the
  repo already uses for the same thing (a worker that is not told invents a second one, e.g.
  a manual migration where the code already adds columns at runtime). If the task may
  rewrite or remove tests, the spec must contain `[allow-test-changes]`; otherwise a
  deleted or skipped test fails the gate.
- `--acceptance`: how the gate will see it is done, in terms of the repo's gate commands
  ("`go test ./...` passes and `TestTwo` exists"). "Works" is not acceptance. When another
  program reads the output, a test of the output's exact shape belongs here: a library
  printing a banner to stdout passes every unit test and breaks the reader.
- `--files`: the files (or `dir/` prefixes) the task may write. Two tasks running at once
  must not share a file. Lock files, migrations and generated code belong to one task.
- `--tier`: `A` for hard work, `B` for routine. `S` is reserved for the lead.
- `--needs`: ids of tasks that must merge first (it waits as draft). Use it instead of
  hoping two tasks run in the right order.
- `--runs-on`: node labels the machine must have, when the work needs what only some
  machines have (`vpc` for a private database, `gpu`). Leave it out otherwise: any machine
  with a free account takes it, and that is what spreads the work.

```sh
swarm task add --repo app --title "..." --spec "..." --acceptance "..." --files a.go,a_test.go
```

Several tasks of one change: add the first, then pass `--run <id>` to the rest so they
share a run.

## 3. Run

```sh
swarm daemon --until-idle     # foreground; exits when nothing is left it can move
swarm status
```

Check `swarm status` before claiming anything is done. `merged` means the gate passed and
`main` moved (with `merge = "branch"`: the delivery branch exists; a person ships it).

A task that stays `ready`: `swarm status` shows `held since ...` when the host's admit
command says not now (another job runs, memory is short), and `swarm providers` shows a
provider cooling until a reset or over its budget. Both clear by themselves; do not retry.
With nodes, `swarm status` lists each one: a task whose `--runs-on` only an offline node
has waits for it, and a task stays on the node of its previous attempt while that node is
connected (its worktree is there). Machines and the link: `docs/nodes.md` in the repo.

## 4. When a task is blocked

`blocked` means out of attempts, the infrastructure failed it three times, or the worker
asked a question. Read why before retrying:

```sh
swarm task notes <id>              # gate failures, plans, questions, newest first
swarm task list --status blocked
sqlite3 "$SWARM_PROFILE/state/swarm.db" \
  "SELECT type, data FROM events WHERE task_id=<id> ORDER BY id DESC LIMIT 10"
```

The fix is almost always in the task, not in the worker: a spec that named no files, an
acceptance the gate cannot see, a task too big. Rewrite it and add it again, or split it.
The most common question is a file outside the task's claims: a test that asserts the very
behaviour the task changes. When the worker is right, widen the task instead of rewriting
it: `swarm task claim <id> <path>` (MCP `swarm_claim_files`), then `swarm task retry <id>`.
Name such tests in `--files` up front next time: the spec changes them, so the task owns them.
Attempt logs: `$SWARM_PROFILE/state/attempts/<provider>-T<id>-<n>/log.jsonl`.

## Rules

- Never weaken the repo's gate to get a task through. A red gate is the answer.
- Never push what swarm merged unless the user asked; swarm itself never pushes.
- Do not run two daemons on one profile.
- Spending: every worker spends the provider's quota. Before a large run, say how many
  tasks and which tiers, and check the user's remaining limits if a tool shows them.
