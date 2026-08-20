-- name: CreateRecipeDraft :exec
INSERT INTO recipe_drafts
(id, state, source, resolved_commit, resolved_tree, manifest, candidates,
 selected_assets, diagnostics, package_digest, run_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetRecipeDraft :one
SELECT id, version, state, source, resolved_commit, resolved_tree, manifest,
       candidates, selected_assets, diagnostics, package_digest, run_id,
       created_at, updated_at
FROM recipe_drafts WHERE id = ?;

-- name: ListRecipeDrafts :many
SELECT id, version, state, source, resolved_commit, resolved_tree, manifest,
       candidates, selected_assets, diagnostics, package_digest, run_id,
       created_at, updated_at
FROM recipe_drafts ORDER BY updated_at DESC;

-- name: UpdateRecipeDraft :execrows
UPDATE recipe_drafts
SET version = version + 1, state = ?, manifest = ?, selected_assets = ?,
    diagnostics = ?, package_digest = ?, run_id = ?,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND version = ?;

-- name: DeleteRecipeDraft :exec
DELETE FROM recipe_drafts WHERE id = ?;
