CREATE TABLE recipe_repositories (
    id                   TEXT PRIMARY KEY,
    source_url           TEXT NOT NULL,
    source_path          TEXT NOT NULL,
    tracking_ref         TEXT NOT NULL DEFAULT 'HEAD',
    current_digest       TEXT REFERENCES recipes(digest) ON DELETE RESTRICT,
    observed_head_commit TEXT,
    observed_head_tree   TEXT,
    head_checked_at      TEXT,
    created_at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (source_url, source_path)
);

CREATE TABLE recipe_repository_versions (
    repository_id TEXT NOT NULL REFERENCES recipe_repositories(id) ON DELETE CASCADE,
    recipe_digest TEXT NOT NULL REFERENCES recipes(digest) ON DELETE RESTRICT,
    commit_sha    TEXT NOT NULL,
    tree_sha      TEXT,
    canonical     INTEGER NOT NULL DEFAULT 1 CHECK (canonical IN (0, 1)),
    installed_at  TEXT NOT NULL,
    PRIMARY KEY (repository_id, recipe_digest)
);

CREATE TEMP TABLE _recipe_source_backfill AS
WITH extracted AS (
    SELECT
        r.digest,
        r.installed_at,
        json_extract(r.manifest, '$.metadata.source.url') AS raw_url,
        COALESCE(json_extract(r.manifest, '$.metadata.source.path'), '') AS raw_path,
        lower(COALESCE(json_extract(r.manifest, '$.metadata.source.revision'), '')) AS commit_sha,
        COALESCE(json_extract(r.source, '$.tree'), '') AS tree_sha
    FROM recipes r
    WHERE COALESCE(json_extract(r.manifest, '$.metadata.source.url'), '') != ''
), github_normalized AS (
    SELECT
        digest,
        installed_at,
        CASE
            WHEN lower(substr(rtrim(raw_url, '/'), 1, 19)) = 'https://github.com/'
                THEN 'https://github.com/' || substr(rtrim(raw_url, '/'), 20)
            ELSE rtrim(raw_url, '/')
        END AS slashless_url,
        CASE
            WHEN trim(raw_path, '/') = '' THEN '.'
            ELSE trim(raw_path, '/')
        END AS source_path,
        commit_sha,
        NULLIF(tree_sha, '') AS tree_sha
    FROM extracted
)
SELECT
    digest,
    installed_at,
    CASE
        WHEN lower(substr(slashless_url, -4)) = '.git'
            THEN substr(slashless_url, 1, length(slashless_url) - 4)
        ELSE slashless_url
    END AS source_url,
    source_path,
    commit_sha,
    tree_sha
FROM github_normalized
WHERE commit_sha != '';

INSERT INTO recipe_repositories (id, source_url, source_path, created_at, updated_at)
SELECT
    'repo-' || lower(hex(source_url || char(10) || source_path)),
    source_url,
    source_path,
    MIN(installed_at),
    MAX(installed_at)
FROM _recipe_source_backfill
GROUP BY source_url, source_path;

INSERT INTO recipe_repository_versions (
    repository_id, recipe_digest, commit_sha, tree_sha, canonical, installed_at
)
SELECT
    'repo-' || lower(hex(b.source_url || char(10) || b.source_path)),
    b.digest,
    b.commit_sha,
    b.tree_sha,
    CASE WHEN b.digest = (
        SELECT b2.digest
        FROM _recipe_source_backfill b2
        WHERE b2.source_url = b.source_url
          AND b2.source_path = b.source_path
          AND b2.commit_sha = b.commit_sha
        ORDER BY b2.installed_at DESC, b2.digest DESC
        LIMIT 1
    ) THEN 1 ELSE 0 END,
    b.installed_at
FROM _recipe_source_backfill b;

CREATE UNIQUE INDEX idx_recipe_repository_commit_canonical
    ON recipe_repository_versions(repository_id, commit_sha)
    WHERE canonical = 1;
CREATE INDEX idx_recipe_repository_versions_digest
    ON recipe_repository_versions(recipe_digest);

UPDATE recipe_repositories
SET current_digest = (
    SELECT v.recipe_digest
    FROM recipe_repository_versions v
    WHERE v.repository_id = recipe_repositories.id
      AND v.canonical = 1
    ORDER BY v.installed_at DESC, v.recipe_digest DESC
    LIMIT 1
);

UPDATE recipe_repositories
SET
    tracking_ref = COALESCE((
        SELECT c.tracking_ref
        FROM recipe_update_checks c
        JOIN recipe_repository_versions v ON v.recipe_digest = c.recipe_digest
        WHERE v.repository_id = recipe_repositories.id
        ORDER BY c.checked_at DESC, c.recipe_digest DESC
        LIMIT 1
    ), 'HEAD'),
    observed_head_commit = (
        SELECT c.candidate_revision
        FROM recipe_update_checks c
        JOIN recipe_repository_versions v ON v.recipe_digest = c.recipe_digest
        WHERE v.repository_id = recipe_repositories.id
        ORDER BY c.checked_at DESC, c.recipe_digest DESC
        LIMIT 1
    ),
    head_checked_at = (
        SELECT c.checked_at
        FROM recipe_update_checks c
        JOIN recipe_repository_versions v ON v.recipe_digest = c.recipe_digest
        WHERE v.repository_id = recipe_repositories.id
        ORDER BY c.checked_at DESC, c.recipe_digest DESC
        LIMIT 1
    );

DROP TABLE recipe_update_checks;
DROP TABLE _recipe_source_backfill;
