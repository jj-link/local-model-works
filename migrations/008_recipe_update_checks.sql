CREATE TABLE IF NOT EXISTS recipe_update_checks (
    recipe_digest      TEXT PRIMARY KEY REFERENCES recipes(digest) ON DELETE CASCADE,
    remote             TEXT NOT NULL,
    tracking_ref       TEXT NOT NULL,
    path               TEXT NOT NULL DEFAULT '',
    installed_revision TEXT NOT NULL,
    candidate_revision TEXT,
    state              TEXT NOT NULL CHECK (state IN ('current', 'available', 'error')),
    checked_at         TEXT NOT NULL,
    error              TEXT
);

CREATE INDEX IF NOT EXISTS idx_recipe_update_checks_checked
    ON recipe_update_checks(checked_at);
