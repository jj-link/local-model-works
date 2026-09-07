-- name: CreateRecipeDraft :exec
INSERT INTO recipe_drafts
(id, state, source, resolved_commit, resolved_tree, manifest, candidates,
 selected_assets, diagnostics, package_digest, run_id, operation, proposal,
 context_selection, questions, acknowledged_warnings, resolved_references,
 parent_draft_id, change_context)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetRecipeDraft :one
SELECT id, version, state, source, resolved_commit, resolved_tree, manifest,
       candidates, selected_assets, diagnostics, package_digest, run_id,
       created_at, updated_at, operation, proposal, context_selection,
       questions, acknowledged_warnings, resolved_references, parent_draft_id, change_context
FROM recipe_drafts WHERE id = ?;

-- name: ListRecipeDrafts :many
SELECT id, version, state, source, resolved_commit, resolved_tree, manifest,
       candidates, selected_assets, diagnostics, package_digest, run_id,
       created_at, updated_at, operation, proposal, context_selection,
       questions, acknowledged_warnings, resolved_references, parent_draft_id, change_context
FROM recipe_drafts ORDER BY updated_at DESC;

-- name: ListRecipeDraftsByPackageDigest :many
SELECT id, version, state, source, resolved_commit, resolved_tree, manifest,
       candidates, selected_assets, diagnostics, package_digest, run_id,
       created_at, updated_at, operation, proposal, context_selection,
       questions, acknowledged_warnings, resolved_references, parent_draft_id, change_context
FROM recipe_drafts
WHERE package_digest = ?
ORDER BY updated_at DESC;

-- name: ListRecipeDraftsByRepository :many
SELECT id, version, state, source, resolved_commit, resolved_tree, manifest,
       candidates, selected_assets, diagnostics, package_digest, run_id,
       created_at, updated_at, operation, proposal, context_selection,
       questions, acknowledged_warnings, resolved_references, parent_draft_id, change_context
FROM recipe_drafts
WHERE json_extract(change_context, '$.repository_id') = ?
ORDER BY updated_at DESC;

-- name: UpdateRecipeDraft :execrows
UPDATE recipe_drafts
SET version = version + 1, state = ?, source = ?, resolved_commit = ?,
    resolved_tree = ?, manifest = ?, candidates = ?, selected_assets = ?,
    diagnostics = ?, package_digest = ?, run_id = ?, proposal = ?,
    context_selection = ?, questions = ?, acknowledged_warnings = ?,
    resolved_references = ?, parent_draft_id = ?, change_context = ?,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND version = ? AND operation IS NULL;

-- name: ReserveRecipeDraftOperation :execrows
UPDATE recipe_drafts
SET version = version + 1, state = 'analyzing', operation = ?,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND version = ? AND operation IS NULL;

-- name: AttachRecipeDraftOperationRun :execrows
UPDATE recipe_drafts
SET run_id = ?,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND json_extract(operation, '$.id') = ?;

-- name: UpdateRecipeDraftOperation :execrows
UPDATE recipe_drafts
SET operation = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND json_extract(operation, '$.id') = ?;

-- name: PinRecipeDraftSource :execrows
UPDATE recipe_drafts
SET resolved_commit = ?, resolved_tree = ?,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND json_extract(operation, '$.id') = ?
  AND (resolved_commit IS NULL OR resolved_commit = ?);

-- name: CompleteRecipeDraftOperation :execrows
UPDATE recipe_drafts
SET version = version + 1, state = ?, source = ?, resolved_commit = ?,
    resolved_tree = ?, manifest = ?, candidates = ?, selected_assets = ?,
    diagnostics = ?, package_digest = ?, run_id = ?, operation = NULL,
    proposal = ?, context_selection = ?, questions = ?,
    acknowledged_warnings = ?, resolved_references = ?, parent_draft_id = ?, change_context = ?,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND json_extract(operation, '$.id') = ?;

-- name: DeleteRecipeDraft :execrows
DELETE FROM recipe_drafts
WHERE id = ? AND version = ? AND operation IS NULL;
