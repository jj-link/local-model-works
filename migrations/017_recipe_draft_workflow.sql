ALTER TABLE recipe_drafts ADD COLUMN operation TEXT;
ALTER TABLE recipe_drafts ADD COLUMN proposal TEXT;
ALTER TABLE recipe_drafts ADD COLUMN context_selection TEXT NOT NULL DEFAULT '[]';
ALTER TABLE recipe_drafts ADD COLUMN questions TEXT NOT NULL DEFAULT '[]';
ALTER TABLE recipe_drafts ADD COLUMN acknowledged_warnings TEXT NOT NULL DEFAULT '[]';
ALTER TABLE recipe_drafts ADD COLUMN resolved_references TEXT NOT NULL DEFAULT '[]';
ALTER TABLE recipe_drafts ADD COLUMN parent_draft_id TEXT REFERENCES recipe_drafts(id) ON DELETE SET NULL;

-- Preserve an actionable blocker for every legacy selection that cannot be
-- associated with an inventoried path before converting the old hash array.
UPDATE recipe_drafts
SET diagnostics = (
    SELECT json_group_array(json(item))
    FROM (
        SELECT existing.value AS item
        FROM json_each(recipe_drafts.diagnostics) AS existing
        UNION ALL
        SELECT json_object(
            'id', 'recipe.asset_selection_missing:' || selected.value,
            'code', 'recipe.asset_selection_missing',
            'severity', 'error',
            'message', 'Previously selected asset bytes are no longer present in the retained inventory.',
            'phase', 'validate',
            'blocking', json('true'),
            'remediation', 'Select an available path and verified hash before packaging.',
            'retryable', json('false'),
            'dismissible', json('false')
        ) AS item
        FROM json_each(recipe_drafts.selected_assets) AS selected
        WHERE NOT EXISTS (
            SELECT 1
            FROM json_each(recipe_drafts.candidates) AS candidate
            WHERE json_extract(candidate.value, '$.sha256') = selected.value
        )
    )
)
WHERE EXISTS (
    SELECT 1
    FROM json_each(recipe_drafts.selected_assets) AS selected
    WHERE NOT EXISTS (
        SELECT 1
        FROM json_each(recipe_drafts.candidates) AS candidate
        WHERE json_extract(candidate.value, '$.sha256') = selected.value
    )
);

-- Old candidates came only from inspected source. Preserve every existing key
-- while adding the explicit provenance discriminator.
UPDATE recipe_drafts
SET candidates = COALESCE((
    SELECT json_group_array(json(json_set(candidate.value, '$.origin',
        COALESCE(json_extract(candidate.value, '$.origin'), 'source'))))
    FROM json_each(recipe_drafts.candidates) AS candidate
), '[]');

-- One legacy hash selected every matching pathname. Expand it that way so
-- identical bytes at different paths remain selected after the cutover.
UPDATE recipe_drafts
SET selected_assets = COALESCE((
    SELECT json_group_array(json_object(
        'path', json_extract(candidate.value, '$.path'),
        'sha256', json_extract(candidate.value, '$.sha256')
    ))
    FROM json_each(recipe_drafts.candidates) AS candidate
    JOIN json_each(recipe_drafts.selected_assets) AS selected
      ON json_extract(candidate.value, '$.sha256') = selected.value
), '[]');

CREATE INDEX idx_recipe_drafts_package_digest
    ON recipe_drafts(package_digest, updated_at DESC);
