-- Item, destination lock and command/transfer ownership must be committed before
-- dispatch. Transactions use these same queries for standalone and serving work.

-- name: CreateDownloadItem :exec
INSERT INTO download_items (
    id, run_id, predecessor_item_id, resource_key, node_id, resource_json
) VALUES (
    sqlc.arg(id), sqlc.arg(run_id), sqlc.narg(predecessor_item_id),
    sqlc.arg(resource_key), sqlc.arg(node_id), sqlc.arg(resource_json)
);

-- name: GetDownloadItem :one
SELECT id, run_id, predecessor_item_id, resource_key, node_id, resource_json,
       state, command_id, transfer_id, checkpoint_json, error_json, updated_at
FROM download_items WHERE id = ?;

-- name: GetDownloadItemByCommand :one
SELECT id, run_id, predecessor_item_id, resource_key, node_id, resource_json,
       state, command_id, transfer_id, checkpoint_json, error_json, updated_at
FROM download_items WHERE command_id = ? AND node_id = ?;

-- name: GetDownloadItemByTransfer :one
SELECT id, run_id, predecessor_item_id, resource_key, node_id, resource_json,
       state, command_id, transfer_id, checkpoint_json, error_json, updated_at
FROM download_items WHERE transfer_id = ? AND node_id = ?;

-- name: ListDownloadItems :many
SELECT id, run_id, predecessor_item_id, resource_key, node_id, resource_json,
       state, command_id, transfer_id, checkpoint_json, error_json, updated_at
FROM download_items WHERE run_id = ? ORDER BY resource_key;

-- name: ListActiveDownloadItemsByNode :many
SELECT id, run_id, predecessor_item_id, resource_key, node_id, resource_json,
       state, command_id, transfer_id, checkpoint_json, error_json, updated_at
FROM download_items
WHERE node_id = ? AND state IN ('pending', 'checking', 'transferring', 'verifying', 'cancelling')
ORDER BY run_id, resource_key;

-- name: BindDownloadItemCommand :execrows
UPDATE download_items
SET command_id = sqlc.arg(command_id),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE download_items.id = sqlc.arg(id) AND download_items.node_id = sqlc.arg(node_id)
    AND download_items.state = sqlc.arg(expected_state) AND download_items.state IN ('pending', 'checking', 'transferring', 'verifying')
    AND download_items.command_id IS NULL
    AND EXISTS (
        SELECT 1 FROM acquisition_destination_locks AS lock
        WHERE lock.item_id = download_items.id AND lock.state = 'held'
    );

-- name: BindDownloadItemTransfer :execrows
UPDATE download_items
SET transfer_id = sqlc.arg(transfer_id),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE download_items.id = sqlc.arg(id) AND download_items.node_id = sqlc.arg(node_id)
    AND download_items.state = sqlc.arg(expected_state) AND download_items.state IN ('pending', 'checking', 'transferring', 'verifying')
    AND download_items.transfer_id IS NULL
    AND EXISTS (
        SELECT 1 FROM acquisition_destination_locks AS lock
        WHERE lock.item_id = download_items.id AND lock.state = 'held'
    );

-- State transitions are CAS and additionally guarded by the migration trigger.
-- Sender node and both attempt IDs bind results to the currently owned attempt.
-- NULL IDs must match NULL, never act as wildcards. Zero affected rows means a
-- stale result or concurrent transition; it is not permission to retry a fetch.
-- name: TransitionDownloadItem :execrows
UPDATE download_items
SET state = sqlc.arg(state), checkpoint_json = sqlc.arg(checkpoint_json),
    error_json = sqlc.narg(error_json),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id) AND node_id = sqlc.arg(node_id)
    AND state = sqlc.arg(expected_state)
    AND state IN ('pending', 'checking', 'transferring', 'verifying', 'cancelling')
    AND command_id IS sqlc.narg(expected_command_id)
    AND transfer_id IS sqlc.narg(expected_transfer_id);

-- Checkpoint CAS prevents an older progress observation from overwriting a newer
-- one. The coordinator validates counters/phase before attempting this update.
-- name: RecordDownloadItemCheckpoint :execrows
UPDATE download_items
SET checkpoint_json = sqlc.arg(checkpoint_json),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id) AND node_id = sqlc.arg(node_id)
    AND state = sqlc.arg(expected_state)
    AND state IN ('checking', 'transferring', 'verifying', 'cancelling')
    AND command_id IS sqlc.narg(expected_command_id)
    AND transfer_id IS sqlc.narg(expected_transfer_id)
    AND checkpoint_json = sqlc.arg(expected_checkpoint_json);

-- A restart interrupts the attempt without clearing command IDs or locks.
-- Reconciliation must obtain agent quiescence before any successor can dispatch.
-- name: InterruptDownloadItemsForRun :execrows
UPDATE download_items
SET state = 'interrupted', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE run_id = ? AND state IN ('pending', 'checking', 'transferring', 'verifying', 'cancelling');

-- name: AcquireDestinationLock :exec
INSERT INTO acquisition_destination_locks (
    node_id, destination, identity, platform, owner_kind, owner_id, run_id, item_id
) VALUES (
    sqlc.arg(node_id), sqlc.arg(destination), sqlc.arg(identity), sqlc.arg(platform),
    sqlc.arg(owner_kind), sqlc.arg(owner_id), sqlc.narg(run_id), sqlc.narg(item_id)
);

-- name: GetDestinationLock :one
SELECT node_id, destination, identity, platform, owner_kind, owner_id,
       run_id, item_id, state, quiesced_at, quiescence_proof, created_at, updated_at
FROM acquisition_destination_locks WHERE node_id = ? AND destination = ?;

-- name: ListDestinationLocksByOwner :many
SELECT node_id, destination, identity, platform, owner_kind, owner_id,
       run_id, item_id, state, quiesced_at, quiescence_proof, created_at, updated_at
FROM acquisition_destination_locks
WHERE owner_kind = ? AND owner_id = ? ORDER BY node_id, destination;

-- name: ListDestinationLocksByRun :many
SELECT node_id, destination, identity, platform, owner_kind, owner_id,
       run_id, item_id, state, quiesced_at, quiescence_proof, created_at, updated_at
FROM acquisition_destination_locks WHERE run_id = ? ORDER BY node_id, destination;

-- name: ListUnquiescedDestinationLocksByNode :many
SELECT node_id, destination, identity, platform, owner_kind, owner_id,
       run_id, item_id, state, quiesced_at, quiescence_proof, created_at, updated_at
FROM acquisition_destination_locks
WHERE node_id = ? AND state <> 'quiesced' ORDER BY destination;

-- name: CancelDestinationLock :execrows
UPDATE acquisition_destination_locks
SET state = 'cancelling', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE node_id = sqlc.arg(node_id) AND destination = sqlc.arg(destination)
    AND owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id)
    AND state = 'held';

-- Call only after an enrolled node's matching terminal acknowledgement or a
-- bounded node quiescence barrier. never-dispatched is allowed only when durable
-- command ownership proves no dispatch could have occurred. No timeout/offline
-- inference is a quiescence proof; cancelling stays pending until confirmed.
-- name: QuiesceDestinationLock :execrows
UPDATE acquisition_destination_locks
SET state = 'quiesced', quiesced_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    quiescence_proof = sqlc.arg(quiescence_proof),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE node_id = sqlc.arg(node_id) AND destination = sqlc.arg(destination)
    AND owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id)
    AND state = sqlc.arg(expected_state) AND state IN ('held', 'cancelling');

-- Delete/reacquire for a successor in one transaction after quiescence. The
-- immutable old item and its checkpoint remain available for explicit Resume.
-- name: ReleaseDestinationLock :execrows
DELETE FROM acquisition_destination_locks
WHERE node_id = sqlc.arg(node_id) AND destination = sqlc.arg(destination)
    AND owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id)
    AND state = 'quiesced';

-- Listing retains every matching attempt, not merely the latest request. The
-- input includes normalized variants, workload, targets and predecessor linkage.
-- name: ListRecipeDownloadRuns :many
SELECT id, module, kind, state, resources, input, output, error_code, error_message,
       deployment_id, legacy_identity, created_at, started_at, finished_at
FROM runs
WHERE module = 'library' AND kind = 'recipe-download'
    AND json_extract(input, '$.plan.recipe_digest') = ?
ORDER BY created_at DESC, id DESC;

-- name: GetRecipeDownloadRun :one
SELECT id, module, kind, state, resources, input, output, error_code, error_message,
       deployment_id, legacy_identity, created_at, started_at, finished_at
FROM runs
WHERE id = ? AND module = 'library' AND kind = 'recipe-download'
    AND json_extract(input, '$.plan.recipe_digest') = ?;
