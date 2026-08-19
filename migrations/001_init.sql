-- Local Model Works — initial schema (forward-only, transactional).

PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS users (
    username     TEXT PRIMARY KEY,
    argon2_hash  TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash  TEXT PRIMARY KEY,
    username    TEXT NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    csrf_hash   TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS enrollment_tokens (
    id            TEXT PRIMARY KEY,
    token_hash    BLOB NOT NULL,
    description   TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at    TEXT NOT NULL,
    used_at       TEXT,
    used_by_node  TEXT REFERENCES nodes(id)
);
CREATE INDEX IF NOT EXISTS idx_enrollment_tokens_hash ON enrollment_tokens(token_hash);

CREATE TABLE IF NOT EXISTS nodes (
    id                       TEXT PRIMARY KEY,
    display_name             TEXT NOT NULL,
    labels                   TEXT NOT NULL DEFAULT '{}',
    agent_version            TEXT,
    status                   TEXT NOT NULL DEFAULT 'pending',
    last_heartbeat           TEXT,
    inventory                TEXT,
    certificate_expires_at   TEXT,
    created_at               TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_nodes_status ON nodes(status);

CREATE TABLE IF NOT EXISTS fabrics (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    transport      TEXT NOT NULL CHECK (transport IN ('roce', 'tcp')),
    interface_name TEXT,
    address        TEXT,
    rdma_device    TEXT,
    members        TEXT NOT NULL,
    state          TEXT NOT NULL DEFAULT 'incomplete',
    diagnostics    TEXT NOT NULL DEFAULT '[]',
    version        TEXT NOT NULL,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS artifacts (
    id               TEXT PRIMARY KEY,
    kind             TEXT NOT NULL,
    identity         TEXT NOT NULL UNIQUE,
    revision         TEXT,
    digest           TEXT,
    validation_state TEXT NOT NULL DEFAULT 'pending',
    metadata         TEXT NOT NULL DEFAULT '{}',
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_artifacts_kind ON artifacts(kind);

CREATE TABLE IF NOT EXISTS artifact_placements (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    artifact_id TEXT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    node_id     TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    state       TEXT NOT NULL DEFAULT 'pending',
    verified_at TEXT,
    diagnostics TEXT NOT NULL DEFAULT '[]',
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    UNIQUE (artifact_id, node_id, path)
);
CREATE INDEX IF NOT EXISTS idx_placements_node ON artifact_placements(node_id);

CREATE TABLE IF NOT EXISTS recipes (
    digest        TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    version       TEXT NOT NULL,
    display_name  TEXT,
    description   TEXT,
    license       TEXT,
    source        TEXT NOT NULL DEFAULT '{}',
    trust_state   TEXT NOT NULL DEFAULT 'untrusted' CHECK (trust_state IN ('verified', 'local', 'untrusted')),
    manifest      TEXT NOT NULL,
    installed_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_recipes_name ON recipes(name);

CREATE TABLE IF NOT EXISTS deployments (
    id                TEXT PRIMARY KEY,
    recipe_digest     TEXT NOT NULL REFERENCES recipes(digest) ON DELETE RESTRICT,
    profile           TEXT NOT NULL,
    placement         TEXT NOT NULL,
    fabric            TEXT,
    desired_state     TEXT NOT NULL DEFAULT 'running' CHECK (desired_state IN ('running', 'stopped')),
    observed_state    TEXT NOT NULL DEFAULT 'unknown',
    endpoint          TEXT,
    model_capabilities TEXT,
    diagnostics       TEXT NOT NULL DEFAULT '[]',
    run_id            TEXT,
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (recipe_digest, profile, placement)
);
CREATE INDEX IF NOT EXISTS idx_deployments_state ON deployments(desired_state, observed_state);

CREATE TABLE IF NOT EXISTS runs (
    id              TEXT PRIMARY KEY,
    module          TEXT NOT NULL,
    kind            TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'queued',
    resources       TEXT NOT NULL DEFAULT '{}',
    input           TEXT NOT NULL DEFAULT '{}',
    output          TEXT,
    error_code      TEXT,
    error_message   TEXT,
    deployment_id   TEXT REFERENCES deployments(id) ON DELETE SET NULL,
    legacy_identity TEXT,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    started_at      TEXT,
    finished_at     TEXT
);
CREATE INDEX IF NOT EXISTS idx_runs_module_state ON runs(module, state);
CREATE INDEX IF NOT EXISTS idx_runs_created ON runs(created_at DESC);

CREATE TABLE IF NOT EXISTS module_settings (
    module    TEXT PRIMARY KEY,
    settings  TEXT NOT NULL DEFAULT '{}',
    version   TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS secrets (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    purpose     TEXT NOT NULL CHECK (purpose IN ('huggingface', 'github', 'registry')),
    nonce       BLOB NOT NULL,
    ciphertext  BLOB NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS transfers (
    id              TEXT PRIMARY KEY,
    artifact_id     TEXT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    source_node     TEXT NOT NULL REFERENCES nodes(id),
    dest_node       TEXT NOT NULL REFERENCES nodes(id),
    dest_path       TEXT NOT NULL,
    credential_hash BLOB,
    state           TEXT NOT NULL DEFAULT 'pending',
    bytes_total     INTEGER NOT NULL DEFAULT 0,
    bytes_done      INTEGER NOT NULL DEFAULT 0,
    diagnostic      TEXT,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_transfers_state ON transfers(state);

CREATE TABLE IF NOT EXISTS benchmark_results (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id            TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    language          TEXT NOT NULL,
    endpoint          TEXT,
    model             TEXT,
    requests          INTEGER NOT NULL DEFAULT 0,
    successes         INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    wall_seconds      REAL NOT NULL DEFAULT 0,
    tokens_per_second REAL NOT NULL DEFAULT 0,
    latency           TEXT NOT NULL DEFAULT '{}',
    first_token       TEXT NOT NULL DEFAULT '{}',
    grading           TEXT,
    quantization      TEXT,
    reasoning         TEXT,
    result_path       TEXT,
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (run_id, language)
);

CREATE TABLE IF NOT EXISTS telemetry_5s (
    node_id  TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ts       INTEGER NOT NULL,
    payload  TEXT NOT NULL,
    PRIMARY KEY (node_id, ts)
);

CREATE TABLE IF NOT EXISTS telemetry_1m (
    node_id  TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ts       INTEGER NOT NULL,
    payload  TEXT NOT NULL,
    PRIMARY KEY (node_id, ts)
);

CREATE TABLE IF NOT EXISTS events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    ts      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    type    TEXT NOT NULL,
    node_id TEXT,
    payload TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(type);

CREATE TABLE IF NOT EXISTS migration_plans (
    plan_digest TEXT PRIMARY KEY,
    request     TEXT NOT NULL,
    plan        TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
