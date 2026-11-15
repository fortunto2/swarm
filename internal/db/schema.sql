-- PRD §5 plus: tasks.repo (one profile spans several repos), nodes (where workers run).
-- Times are Unix seconds, UTC. Only swarmd writes here.

CREATE TABLE IF NOT EXISTS runs(
  id INTEGER PRIMARY KEY, spec_path TEXT NOT NULL, topic TEXT,
  status TEXT NOT NULL CHECK(status IN('planning','running','paused','done','failed')),
  base_commit TEXT NOT NULL, created_at INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS tasks(
  id INTEGER PRIMARY KEY, run_id INTEGER NOT NULL REFERENCES runs(id),
  parent_id INTEGER REFERENCES tasks(id), depth INTEGER NOT NULL DEFAULT 0,
  repo TEXT NOT NULL,
  title TEXT NOT NULL, spec TEXT NOT NULL, acceptance TEXT NOT NULL,
  tier TEXT NOT NULL CHECK(tier IN('S','A','B')),
  priority INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL CHECK(status IN('draft','ready','running','verifying',
    'review','merging','merged','split','blocked','needs_human','failed','cancelled')),
  provider TEXT, model TEXT, worker_id TEXT, node TEXT,
  branch TEXT, worktree TEXT, result_commit TEXT,
  attempts INTEGER NOT NULL DEFAULT 0,
  infra_failures INTEGER NOT NULL DEFAULT 0,
  lease_until INTEGER, not_before INTEGER,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
DROP INDEX IF EXISTS tasks_sched; -- was (run_id, status, ...): no tick query filters by run_id
CREATE INDEX IF NOT EXISTS tasks_by_status ON tasks(status, priority DESC, id);

CREATE TABLE IF NOT EXISTS deps(
  task_id INTEGER NOT NULL REFERENCES tasks(id), needs_id INTEGER NOT NULL REFERENCES tasks(id),
  PRIMARY KEY(task_id, needs_id));

CREATE TABLE IF NOT EXISTS file_claims(
  path TEXT NOT NULL, task_id INTEGER NOT NULL REFERENCES tasks(id),
  mode TEXT NOT NULL CHECK(mode IN('write','read')), PRIMARY KEY(path, task_id));

CREATE TABLE IF NOT EXISTS notes(
  id INTEGER PRIMARY KEY, run_id INTEGER NOT NULL, task_id INTEGER,
  author TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN('announce','plan','progress','finding','question','review')),
  body TEXT NOT NULL CHECK(length(body) <= 8192), created_at INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS attempts(
  id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL, provider TEXT NOT NULL, model TEXT, node TEXT,
  started_at INTEGER, ended_at INTEGER,
  outcome TEXT CHECK(outcome IN('done','gate_fail','review_fail','timeout','quota','crash','blocked')),
  log_path TEXT, tokens_in INTEGER, tokens_out INTEGER, cost_usd REAL);

CREATE TABLE IF NOT EXISTS providers(
  id TEXT PRIMARY KEY, state TEXT NOT NULL CHECK(state IN('ok','cooling','disabled')),
  cooling_until INTEGER, backoff_s INTEGER NOT NULL DEFAULT 60, last_error TEXT);

CREATE TABLE IF NOT EXISTS nodes(
  id TEXT PRIMARY KEY, state TEXT NOT NULL CHECK(state IN('ok','down','drained')),
  last_seen INTEGER, last_error TEXT);

CREATE TABLE IF NOT EXISTS quota_events(
  id INTEGER PRIMARY KEY, provider TEXT NOT NULL, at INTEGER NOT NULL,
  kind TEXT NOT NULL, resets_at INTEGER, raw TEXT);

CREATE TABLE IF NOT EXISTS events(
  id INTEGER PRIMARY KEY, at INTEGER NOT NULL, actor TEXT NOT NULL,
  task_id INTEGER, type TEXT NOT NULL, data TEXT);
