CREATE TABLE secrets_new (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    purpose     TEXT NOT NULL CHECK (purpose IN ('huggingface', 'github', 'registry', 'model_provider', 'ssh')),
    nonce       BLOB NOT NULL,
    ciphertext  BLOB NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO secrets_new (id, name, purpose, nonce, ciphertext, created_at, updated_at)
SELECT id, name, purpose, nonce, ciphertext, created_at, updated_at FROM secrets;
DROP TABLE secrets;
ALTER TABLE secrets_new RENAME TO secrets;

CREATE TABLE autoresearch_projects (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('idea_intake', 'awaiting_idea_selection', 'running', 'paper_editing', 'completed', 'failed')),
    runner_node_id TEXT REFERENCES nodes(id),
    idea_prompt    TEXT NOT NULL DEFAULT '',
    config_json    TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(config_json)),
    version        INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_autoresearch_projects_status_updated ON autoresearch_projects(status, updated_at DESC);

CREATE TABLE autoresearch_sources (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL REFERENCES autoresearch_projects(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN ('arxiv', 'doi', 'url', 'pdf')),
    locator       TEXT NOT NULL,
    title         TEXT,
    metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json)),
    local_path    TEXT,
    sha256        TEXT CHECK (sha256 IS NULL OR length(sha256) = 64),
    status        TEXT NOT NULL CHECK (status IN ('pending', 'ready', 'blocked', 'failed')),
    error         TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE(project_id, kind, locator)
);
CREATE INDEX idx_autoresearch_sources_project_created ON autoresearch_sources(project_id, created_at);

CREATE TABLE autoresearch_ideas (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES autoresearch_projects(id) ON DELETE CASCADE,
    ordinal    INTEGER NOT NULL CHECK (ordinal BETWEEN 1 AND 10),
    source     TEXT NOT NULL CHECK (source IN ('human', 'generated')),
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    selected   INTEGER NOT NULL DEFAULT 0 CHECK (selected IN (0, 1)),
    version    INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE(project_id, ordinal)
);
CREATE INDEX idx_autoresearch_ideas_project_selected ON autoresearch_ideas(project_id, selected, ordinal);

CREATE TABLE autoresearch_runs (
    run_id                TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    project_id            TEXT NOT NULL REFERENCES autoresearch_projects(id) ON DELETE CASCADE,
    factory               TEXT NOT NULL CHECK (factory IN ('idea', 'proposal', 'deep_lit', 'experiment', 'paper')),
    parent_run_id         TEXT REFERENCES autoresearch_runs(run_id),
    dispatcher_session_id TEXT,
    worker_node_id        TEXT REFERENCES nodes(id),
    config_snapshot       TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(config_snapshot))
);
CREATE INDEX idx_autoresearch_runs_project ON autoresearch_runs(project_id, run_id);

CREATE TABLE autoresearch_invocations (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES autoresearch_runs(run_id) ON DELETE CASCADE,
    parent_id     TEXT REFERENCES autoresearch_invocations(id),
    node_id       TEXT REFERENCES nodes(id),
    role          TEXT NOT NULL,
    backend       TEXT NOT NULL,
    model         TEXT NOT NULL,
    advisor       INTEGER NOT NULL DEFAULT 0 CHECK (advisor IN (0, 1)),
    session_id    TEXT,
    state         TEXT NOT NULL CHECK (state IN ('queued', 'running', 'completed', 'failed')),
    input_tokens  INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cost_usd      REAL NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    started_at    TEXT,
    finished_at   TEXT,
    error         TEXT
);
CREATE INDEX idx_autoresearch_invocations_run_started ON autoresearch_invocations(run_id, started_at, id);

CREATE TABLE autoresearch_messages (
    id                 TEXT PRIMARY KEY,
    project_id         TEXT NOT NULL REFERENCES autoresearch_projects(id) ON DELETE CASCADE,
    role               TEXT NOT NULL CHECK (role IN ('human', 'writer')),
    body               TEXT NOT NULL,
    changed_paths_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(changed_paths_json)),
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_autoresearch_messages_project_created ON autoresearch_messages(project_id, created_at, id);
