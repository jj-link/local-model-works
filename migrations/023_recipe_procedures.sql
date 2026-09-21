-- Rebuild the catalog and its child together to retain foreign-key ownership.
CREATE TEMP TABLE retained_recipe_versions AS SELECT * FROM recipe_repository_versions;
DROP TABLE recipe_repository_versions;
CREATE TABLE recipe_repositories_next (
    id TEXT PRIMARY KEY,
    source_url TEXT NOT NULL,
    source_path TEXT NOT NULL,
    tracking_ref TEXT NOT NULL DEFAULT 'HEAD',
    current_digest TEXT REFERENCES recipes(digest) ON DELETE RESTRICT,
    observed_head_commit TEXT,
    observed_head_tree TEXT,
    head_checked_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    head_check_error TEXT NOT NULL DEFAULT '',
    procedure TEXT NOT NULL DEFAULT '',
    UNIQUE (source_url, source_path, procedure)
);
INSERT INTO recipe_repositories_next (id, source_url, source_path, tracking_ref, current_digest,
    observed_head_commit, observed_head_tree, head_checked_at, created_at, updated_at, head_check_error)
SELECT id, source_url, source_path, tracking_ref, current_digest,
    observed_head_commit, observed_head_tree, head_checked_at, created_at, updated_at, head_check_error
FROM recipe_repositories;
DROP TABLE recipe_repositories;
ALTER TABLE recipe_repositories_next RENAME TO recipe_repositories;
CREATE TABLE recipe_repository_versions (
    repository_id TEXT NOT NULL REFERENCES recipe_repositories(id) ON DELETE CASCADE,
    recipe_digest TEXT NOT NULL REFERENCES recipes(digest) ON DELETE RESTRICT,
    commit_sha TEXT NOT NULL,
    tree_sha TEXT,
    canonical INTEGER NOT NULL DEFAULT 1 CHECK (canonical IN (0, 1)),
    installed_at TEXT NOT NULL,
    PRIMARY KEY (repository_id, recipe_digest)
);
INSERT INTO recipe_repository_versions SELECT * FROM retained_recipe_versions;
DROP TABLE retained_recipe_versions;
CREATE UNIQUE INDEX idx_recipe_repository_commit_canonical
    ON recipe_repository_versions(repository_id, commit_sha) WHERE canonical = 1;
CREATE INDEX idx_recipe_repository_versions_digest ON recipe_repository_versions(recipe_digest);
