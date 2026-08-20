CREATE TABLE recipe_drafts (
    id                TEXT PRIMARY KEY,
    version           INTEGER NOT NULL DEFAULT 1,
    state             TEXT NOT NULL CHECK (state IN ('analyzing','needs_input','valid','packaged','installed','failed')),
    source            TEXT NOT NULL,
    resolved_commit   TEXT,
    resolved_tree     TEXT,
    manifest          TEXT NOT NULL DEFAULT '{}',
    candidates        TEXT NOT NULL DEFAULT '[]',
    selected_assets   TEXT NOT NULL DEFAULT '[]',
    diagnostics       TEXT NOT NULL DEFAULT '[]',
    package_digest    TEXT,
    run_id            TEXT REFERENCES runs(id) ON DELETE SET NULL,
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_recipe_drafts_state ON recipe_drafts(state, updated_at DESC);
