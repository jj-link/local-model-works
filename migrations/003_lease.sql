-- Lease records: the exact resources a deployment or run holds while
-- nonterminal. Ownership is exclusive per resource while active; the
-- partial unique index makes a double-acquire a SQLite constraint error,
-- so concurrent creates serialize inside one transaction instead of
-- relying on read-then-write checks.
CREATE TABLE IF NOT EXISTS leases (
    resource    TEXT NOT NULL,
    owner_kind  TEXT NOT NULL CHECK (owner_kind IN ('deployment', 'run')),
    owner_id    TEXT NOT NULL,
    state       TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'released')),
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    released_at TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_leases_active ON leases(resource) WHERE state = 'active';
CREATE INDEX IF NOT EXISTS idx_leases_owner ON leases(owner_kind, owner_id);
