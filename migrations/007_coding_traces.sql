PRAGMA foreign_keys = OFF;

CREATE TABLE secrets_new (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    purpose     TEXT NOT NULL CHECK (purpose IN ('huggingface', 'github', 'registry', 'model-provider', 'runtime-provider')),
    nonce       BLOB NOT NULL,
    ciphertext  BLOB NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
INSERT INTO secrets_new SELECT * FROM secrets;
DROP TABLE secrets;
ALTER TABLE secrets_new RENAME TO secrets;

PRAGMA foreign_keys = ON;

CREATE TABLE coding_traces (
    id                     TEXT PRIMARY KEY,
    run_id                 TEXT NOT NULL UNIQUE REFERENCES runs(id) ON DELETE CASCADE,
    experiment_id          TEXT REFERENCES swe_gym_experiments(id) ON DELETE SET NULL,
    task_id                TEXT NOT NULL,
    problem                TEXT NOT NULL,
    repository             TEXT NOT NULL,
    base_revision          TEXT NOT NULL,
    model_source           TEXT NOT NULL CHECK (model_source IN ('lmw_deployment','external_api')),
    model                  TEXT NOT NULL,
    scaffold               TEXT NOT NULL DEFAULT 'openhands-codeact',
    sampling               TEXT NOT NULL DEFAULT '{}',
    state                  TEXT NOT NULL CHECK (state IN ('recording','completed','interrupted')),
    final_diff             TEXT,
    verification_id        TEXT,
    success_label          INTEGER CHECK (success_label IN (0,1)),
    failure_kind           TEXT,
    token_count            INTEGER NOT NULL DEFAULT 0 CHECK (token_count >= 0),
    turn_count             INTEGER NOT NULL DEFAULT 0 CHECK (turn_count >= 0),
    pinned                 INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0,1)),
    retain_until           TEXT,
    schema_version         TEXT NOT NULL,
    redaction_version      TEXT NOT NULL,
    redaction_count        INTEGER NOT NULL DEFAULT 0 CHECK (redaction_count >= 0),
    digest                 TEXT,
    created_at             TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at             TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    finished_at            TEXT
);
CREATE INDEX idx_coding_traces_created ON coding_traces(created_at DESC, id DESC);
CREATE INDEX idx_coding_traces_task ON coding_traces(task_id, state, success_label);
CREATE INDEX idx_coding_traces_experiment ON coding_traces(experiment_id, created_at);

CREATE TABLE coding_trace_events (
    trace_id           TEXT NOT NULL REFERENCES coding_traces(id) ON DELETE CASCADE,
    sequence           INTEGER NOT NULL CHECK (sequence >= 0),
    event_id           TEXT NOT NULL,
    agent_id           TEXT NOT NULL DEFAULT '',
    parent_agent_id    TEXT NOT NULL DEFAULT '',
    occurred_at        TEXT NOT NULL,
    kind               TEXT NOT NULL CHECK (kind IN ('agent.lifecycle','message','model.request','model.response','tool.call','tool.result')),
    payload            TEXT NOT NULL,
    input_tokens       INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens      INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    redaction_count    INTEGER NOT NULL DEFAULT 0 CHECK (redaction_count >= 0),
    PRIMARY KEY (trace_id, sequence),
    UNIQUE (trace_id, event_id)
);

CREATE TABLE coding_trace_streams (
    trace_id            TEXT NOT NULL REFERENCES coding_traces(id) ON DELETE CASCADE,
    node_id             TEXT NOT NULL,
    rank                INTEGER NOT NULL,
    source              TEXT NOT NULL,
    byte_offset         INTEGER NOT NULL DEFAULT 0 CHECK (byte_offset >= 0),
    next_event_sequence INTEGER NOT NULL DEFAULT 0 CHECK (next_event_sequence >= 0),
    final_offset        INTEGER CHECK (final_offset >= 0),
    eof_acknowledged    INTEGER NOT NULL DEFAULT 0 CHECK (eof_acknowledged IN (0,1)),
    updated_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (trace_id, node_id, rank, source)
);

CREATE TABLE coding_trace_verifications (
    id                    TEXT PRIMARY KEY,
    trace_id              TEXT NOT NULL UNIQUE REFERENCES coding_traces(id) ON DELETE CASCADE,
    command               TEXT NOT NULL,
    timeout_seconds       INTEGER NOT NULL CHECK (timeout_seconds > 0),
    exit_status           INTEGER,
    stdout                TEXT NOT NULL DEFAULT '',
    stderr                TEXT NOT NULL DEFAULT '',
    fail_to_pass_report   TEXT NOT NULL DEFAULT '{}',
    pass_to_pass_report   TEXT NOT NULL DEFAULT '{}',
    status                TEXT NOT NULL CHECK (status IN ('resolved','unresolved','infrastructure_error')),
    failure_kind          TEXT,
    created_at            TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE swe_gym_experiments (
    id                 TEXT PRIMARY KEY,
    run_id             TEXT REFERENCES runs(id) ON DELETE SET NULL,
    state              TEXT NOT NULL CHECK (state IN ('planned','queued','running','cancelling','cancelled','completed','failed')),
    config             TEXT NOT NULL,
    config_digest      TEXT NOT NULL,
    plan               TEXT NOT NULL,
    plan_digest        TEXT NOT NULL,
    manifest           TEXT NOT NULL DEFAULT '{}',
    total_items        INTEGER NOT NULL DEFAULT 0,
    completed_items    INTEGER NOT NULL DEFAULT 0,
    resolved_items     INTEGER NOT NULL DEFAULT 0,
    unresolved_items   INTEGER NOT NULL DEFAULT 0,
    infrastructure_errors INTEGER NOT NULL DEFAULT 0,
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    finished_at        TEXT
);
CREATE INDEX idx_swe_gym_experiments_created ON swe_gym_experiments(created_at DESC, id DESC);

CREATE TABLE swe_gym_work_items (
    id                 TEXT PRIMARY KEY,
    experiment_id      TEXT NOT NULL REFERENCES swe_gym_experiments(id) ON DELETE CASCADE,
    task_id            TEXT NOT NULL,
    rollout_index      INTEGER NOT NULL CHECK (rollout_index >= 0),
    attempt            INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    state              TEXT NOT NULL CHECK (state IN ('queued','running','grading','resolved','unresolved','infrastructure_error','cancelled')),
    child_run_id       TEXT REFERENCES runs(id) ON DELETE SET NULL,
    trace_id           TEXT REFERENCES coding_traces(id) ON DELETE SET NULL,
    node_id            TEXT,
    output             TEXT,
    error_code         TEXT,
    error_message      TEXT,
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    finished_at        TEXT,
    UNIQUE (experiment_id, task_id, rollout_index)
);
CREATE INDEX idx_swe_gym_work_items_state ON swe_gym_work_items(experiment_id, state, task_id, rollout_index);

CREATE TABLE coding_trace_exports (
    id                 TEXT PRIMARY KEY,
    run_id             TEXT NOT NULL UNIQUE REFERENCES runs(id) ON DELETE CASCADE,
    state              TEXT NOT NULL CHECK (state IN ('queued','running','completed','failed','cancelled')),
    selection          TEXT NOT NULL,
    seed               INTEGER NOT NULL,
    artifact_path      TEXT,
    manifest_digest    TEXT,
    canonical_count    INTEGER NOT NULL DEFAULT 0,
    policy_count       INTEGER NOT NULL DEFAULT 0,
    verifier_count     INTEGER NOT NULL DEFAULT 0,
    excluded_count     INTEGER NOT NULL DEFAULT 0,
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    finished_at        TEXT
);
CREATE INDEX idx_coding_trace_exports_created ON coding_trace_exports(created_at DESC, id DESC);
