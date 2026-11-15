# Nodes: one coordinator, several machines

One database, one daemon that decides, one place sessions ask. Workers run wherever their
accounts are logged in: on the coordinator's own machine, and on **nodes** that dial in.
A laptop that sleeps, a box behind NAT, a rented VPS: each runs `swarm node`, which
connects out to the coordinator, so nothing has to reach in.

```
        sessions (MCP)            ┌──────────── node: laptop ─────────────┐
             │                    │ swarm node ── ssh ──► swarm relay ─┐  │
             ▼                    │   │ worktrees, CLI workers         │  │
  ┌── coordinator (always on) ──┐ │   │ gates (gate_on = "node")       │  │
  │ swarm daemon: the database, │◄┼───┴─ pushes task branches ─────────┘  │
  │ scheduling, gate, merge     │ └───────────────────────────────────────┘
  │ local workers (its accounts)│
  └─────────────────────────────┘
```

## What crosses the link

The link is `ssh <coordinator-host> swarm --profile <p> relay --node`: the relay prints
the operator token on its first line (ssh access already grants reading it) and then
bridges to the daemon's node socket. Over it, three operations:

| Operation | Who | What |
|---|---|---|
| `node_hello` | node | its name, worktree root, repos it has a copy of, attempts still running, worktrees it holds. Answer: a session, its accounts as the coordinator declares them (paths and `secret:` refs resolve on the node), each repo's gate and main ref, worktrees it may delete |
| `node_poll` | node | held up to 25 s; carries liveness, the node's own `limits.admit` answer and its accounts' limit readings. Answer: orders, and where each repo's main is |
| `node_report` | node | started (pid, base), exited (error, log tail), pushed, gate verdict |

Orders: `start` an attempt, `kill` one, `push` the task branch, `gate` a commit, `cleanup`
a worktree, `status` (a task of this node moved: the node runs its own `hooks.on_status`,
with `{worktree}` its path there, so a board or card on that machine follows its tasks). Workers on the node use a local board socket that `swarm node` forwards to
the coordinator, so `swarm gate`, `swarm board` and the board MCP work unchanged.

## Who does what

| | Coordinator | Node |
|---|---|---|
| database, statuses, events | yes, the only writer | never |
| choosing provider and machine | yes | its `limits.admit` can hold it |
| worktree, the CLI process | for local accounts | for its accounts |
| report_done check | after the node pushed the branch | pushes `swarm/T<id>` on request |
| static gate checks | always | |
| gate commands | default | when the repo says `gate_on = "node"` and the node is online |
| rebase, fast-forward, branch delivery | always | fetches deliveries, may fast-forward its main (`land = "ff"`) |

A task stays on the node of its previous attempt while that node is connected: its
worktree, and whatever the attempt committed, are there. A node silent for
`limits.node_timeout` (90 s) gets no new work; after twice that, its running attempts are
lost and go back to ready (an infrastructure failure, not the model's). A node that comes
back stops the workers it no longer owns, including ones it left behind when killed hard.

## Setting it up

The coordinator's profile declares the node and puts accounts on it:

```toml
[nodes.box]          # this machine
host = "local"
accounts = ["claude-box"]
labels = ["vpc"]

[nodes.laptop]       # dials in
host = "remote"
accounts = ["claude-laptop"]   # declared under [providers.claude-laptop] as usual
labels = ["mac"]
max_workers = 3

[repos.app]
path = "~/src/app"
gate = ["make test"]
gate_on = "node"     # the laptop has the cores: its tasks are gated there
```

The node's profile says who it is, how to reach the coordinator, and where its copies are:

```toml
[profile]
name = "laptop"
worktree_root = "~/src/.worktrees"

[node]
name = "laptop"
coordinator = ["ssh", "box", "swarm", "--profile", "/srv/swarm"]

[repos.app]
path = "~/src/app"
remote = "box:src/app"                     # the coordinator's copy, as git reaches it from here
publish = [{ from = "refs/remotes/origin/main", to = "refs/remotes/laptop/main" }]
fetch = "origin"                           # fetched before publishing, every two minutes
```

`publish` is for a coordinator that branches from a ref only the node can refresh (the
node can reach the forge, the coordinator cannot). Pushes are never forced. A big repo over
a slow link: seed the coordinator's copy from a clone already on that machine
(`git -C copy.git fetch /path/to/clone +refs/heads/main:refs/heads/main`), and publish sends
only what is new. A publish may take up to 15 minutes; it runs beside the polls, not in them. `land = "ff"`
fast-forwards the node's main to the coordinator's after merges, when that is a
fast-forward; with `merge = "branch"`, delivered branches are fetched to the node instead.

`swarm --profile <node profile> node --check` proves the link before anything runs: it dials
the coordinator, asks its status and that the node is declared there, and takes no work.
Then `swarm --profile <node profile> node` (a launchd agent or a systemd unit), and a task
can ask for a machine: `swarm task add ... --runs-on vpc`, or `runs_on` in `swarm_add_task`.
`swarm status` on the coordinator shows each node: online or when last seen, running,
holding and why.

## Trust

The link is ssh, and ssh is the authentication: whoever can run `swarm relay --node` on the
coordinator's host is that user, and gets the operator token (it could read it from the
state directory anyway). A node is trusted like the coordinator's own machine. Its workers
are not: they run as the node's user and can write their worktree's shared `.git/config`, so
the node's own git transfers ignore what that config says about ssh commands, credential
helpers and proxies, and refuse a repo whose own config (or a file it includes, or a
worktree's config) rewrites URLs with `url.<x>.insteadOf`; keep such rules in the global
config. A node and a coordinator one release apart still connect: fields that changed shape
got new names, so each side ignores what it does not know. A worker on a node that
holds the user's ssh keys can reach the coordinator the way the node does; give workers
`safety = "strict"` (a sandbox) where that matters.

## Not built

One account on two machines (declare two providers, one per machine). A node offering
repos the coordinator has no copy of. Transports other than a command's stdin/stdout.
