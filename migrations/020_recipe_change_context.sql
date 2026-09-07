ALTER TABLE recipe_drafts ADD COLUMN change_context TEXT
    CHECK (change_context IS NULL OR json_valid(change_context));

-- Only retained package membership proves a repository association. Historical
-- source availability is deliberately not inferred from a stored commit alone.
UPDATE recipe_drafts AS draft
SET change_context = (
    SELECT json_object(
        'kind', CASE WHEN draft.parent_draft_id IS NULL THEN 'add' ELSE 'repair' END,
        'repository_id', version.repository_id,
        'base_recipe_digest', saved.digest,
        'base_commit', version.commit_sha,
        'base_tree', version.tree_sha,
        'base_manifest', json(saved.manifest),
        'base_source_status', 'unavailable'
    )
    FROM recipes AS saved
    JOIN recipe_repository_versions AS version ON version.recipe_digest = saved.digest
    WHERE saved.digest = COALESCE(
        (SELECT parent.package_digest FROM recipe_drafts AS parent WHERE parent.id = draft.parent_draft_id),
        draft.package_digest
    )
    AND (SELECT COUNT(*) FROM recipe_repository_versions AS links WHERE links.recipe_digest = saved.digest) = 1
)
WHERE draft.package_digest IS NOT NULL OR draft.parent_draft_id IS NOT NULL;

CREATE INDEX idx_recipe_drafts_repository
    ON recipe_drafts(json_extract(change_context, '$.repository_id'));
