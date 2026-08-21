-- name: CreateUser :exec
INSERT INTO users (username, argon2_hash) VALUES (?, ?);

-- name: GetUser :one
SELECT username, argon2_hash, created_at FROM users WHERE username = ?;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, username, csrf_hash, created_at, expires_at)
VALUES (?, ?, ?, ?, ?);

-- name: GetSessionByTokenHash :one
SELECT token_hash, username, csrf_hash, created_at, expires_at
FROM sessions WHERE token_hash = ?;

-- name: DeleteSessionByTokenHash :exec
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at < ?;

-- name: CreateEnrollmentToken :exec
INSERT INTO enrollment_tokens (id, token_hash, description, expires_at)
VALUES (?, ?, ?, ?);

-- name: GetEnrollmentTokenByHash :one
SELECT id, description, created_at, expires_at, used_at, used_by_node
FROM enrollment_tokens WHERE token_hash = ?;

-- name: GetEnrollmentTokenByID :one
SELECT id, description, created_at, expires_at, used_at, used_by_node
FROM enrollment_tokens WHERE id = ?;

-- name: ListEnrollmentTokens :many
SELECT id, description, created_at, expires_at, used_at, used_by_node
FROM enrollment_tokens ORDER BY created_at DESC;

-- name: UseEnrollmentToken :one
UPDATE enrollment_tokens
SET used_at = ?, used_by_node = ?
WHERE id = ? AND used_at IS NULL AND expires_at > ?
RETURNING id;

-- name: DeleteEnrollmentToken :exec
DELETE FROM enrollment_tokens WHERE id = ?;

-- name: DeleteExpiredUnusedTokens :exec
DELETE FROM enrollment_tokens WHERE used_at IS NULL AND expires_at < ?;

-- name: CreateNode :exec
INSERT INTO nodes (id, display_name, labels, status, created_at)
VALUES (?, ?, ?, 'pending', ?);

-- name: GetNode :one
SELECT id, display_name, labels, agent_version, status, last_heartbeat, inventory,
       certificate_expires_at, created_at
FROM nodes WHERE id = ?;

-- name: ListNodes :many
SELECT id, display_name, labels, agent_version, status, last_heartbeat, inventory,
       certificate_expires_at, created_at
FROM nodes ORDER BY created_at;

-- name: UpdateNodeMeta :exec
UPDATE nodes SET display_name = ?, labels = ? WHERE id = ?;

-- name: SetNodeInventory :exec
UPDATE nodes SET inventory = ?, agent_version = COALESCE(?, agent_version) WHERE id = ?;

-- name: SetNodeStatus :exec
UPDATE nodes SET status = ?, last_heartbeat = COALESCE(?, last_heartbeat) WHERE id = ?;


-- name: SetNodeVersion :exec
UPDATE nodes SET agent_version = ? WHERE id = ?;
-- name: SetNodeCertificate :exec
UPDATE nodes SET certificate_expires_at = ? WHERE id = ?;

-- name: ApproveNode :exec
UPDATE nodes SET status = 'online' WHERE id = ? AND status = 'pending';

-- name: ListPendingNodes :many
SELECT id, display_name, labels, agent_version, status, last_heartbeat, inventory,
       certificate_expires_at, created_at
FROM nodes WHERE status = 'pending' ORDER BY created_at;

-- name: CreateFabric :exec
INSERT INTO fabrics (id, name, transport, interface_name, address, rdma_device,
                     members, state, diagnostics, version)
VALUES (?, ?, ?, ?, ?, ?, ?, 'incomplete', '[]', ?);

-- name: GetFabric :one
SELECT id, name, transport, interface_name, address, rdma_device, members,
       state, diagnostics, version, created_at, updated_at
FROM fabrics WHERE id = ?;

-- name: GetFabricByIfMatch :one
SELECT id, name, transport, interface_name, address, rdma_device, members,
       state, diagnostics, version, created_at, updated_at
FROM fabrics WHERE id = ? AND version = ?;

-- name: ListFabrics :many
SELECT id, name, transport, interface_name, address, rdma_device, members,
       state, diagnostics, version, created_at, updated_at
FROM fabrics ORDER BY name;

-- name: UpdateFabric :exec
UPDATE fabrics SET transport = ?, interface_name = ?, address = ?, rdma_device = ?,
                   members = ?, state = ?, diagnostics = ?, version = ?,
                   updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND version = ?;

-- name: DeleteFabric :exec
DELETE FROM fabrics WHERE id = ? AND version = ?;

-- name: CreateArtifact :exec
INSERT OR IGNORE INTO artifacts (id, kind, identity, revision, digest, metadata)
VALUES (?, ?, ?, ?, ?, ?);

-- name: GetArtifact :one
SELECT id, kind, identity, revision, digest, validation_state, metadata, created_at
FROM artifacts WHERE id = ?;

-- name: GetArtifactByIdentity :one
SELECT id, kind, identity, revision, digest, validation_state, metadata, created_at
FROM artifacts WHERE identity = ?;

-- name: ListArtifacts :many
SELECT id, kind, identity, revision, digest, validation_state, metadata, created_at
FROM artifacts WHERE (? IS NULL OR kind = ?)
ORDER BY created_at DESC;

-- name: ListArtifactsOnNode :many
SELECT a.id, a.kind, a.identity, a.revision, a.digest, a.validation_state, a.metadata, a.created_at
FROM artifacts a
JOIN artifact_placements p ON p.artifact_id = a.id
WHERE p.node_id = ?
ORDER BY a.created_at DESC;

-- name: SetArtifactValidation :exec
UPDATE artifacts SET validation_state = ? WHERE id = ?;

-- name: UpsertPlacement :exec
INSERT INTO artifact_placements (artifact_id, node_id, path, state, verified_at, diagnostics, size_bytes)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (artifact_id, node_id, path)
DO UPDATE SET state = excluded.state,
              verified_at = excluded.verified_at,
              diagnostics = excluded.diagnostics,
              size_bytes = excluded.size_bytes;

-- name: ListPlacements :many
SELECT id, artifact_id, node_id, path, state, verified_at, diagnostics, size_bytes
FROM artifact_placements WHERE artifact_id = ?;

-- name: ListPlacementsOnNode :many
SELECT id, artifact_id, node_id, path, state, verified_at, diagnostics, size_bytes
FROM artifact_placements WHERE node_id = ?;

-- name: GetPlacement :one
SELECT id, artifact_id, node_id, path, state, verified_at, diagnostics, size_bytes
FROM artifact_placements WHERE artifact_id = ? AND node_id = ? AND path = ?;

-- name: CreateRecipe :exec
INSERT OR IGNORE INTO recipes (digest, name, version, display_name, description,
                               license, source, trust_state, manifest)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetRecipe :one
SELECT digest, name, version, display_name, description, license, source,
       trust_state, manifest, installed_at
FROM recipes WHERE digest = ?;

-- name: ListRecipes :many
SELECT digest, name, version, display_name, description, license, source,
       trust_state, manifest, installed_at
FROM recipes ORDER BY installed_at DESC;

-- name: ListRecipeVersions :many
SELECT digest, name, version, display_name, description, license, source,
       trust_state, manifest, installed_at
FROM recipes WHERE name = ? ORDER BY installed_at DESC;

-- name: UpdateRecipeTrust :exec
UPDATE recipes SET trust_state = ? WHERE digest = ?;

-- name: DeleteRecipe :exec
DELETE FROM recipes WHERE digest = ?;

-- name: RecipeReferencedByDeployments :one
SELECT COUNT(*) FROM deployments WHERE recipe_digest = ?;

-- name: RecipeReferencedByRuns :one
SELECT COUNT(*) FROM runs r
JOIN deployments d ON d.id = r.deployment_id
WHERE d.recipe_digest = ?;

-- name: CreateDeployment :exec
INSERT INTO deployments (id, recipe_digest, profile, placement, fabric,
                         desired_state, observed_state, run_id)
VALUES (?, ?, ?, ?, ?, 'running', 'unknown', ?);

-- name: GetDeployment :one
SELECT id, recipe_digest, profile, placement, fabric, desired_state,
       observed_state, endpoint, model_capabilities, diagnostics, run_id,
       dispatch, created_at, updated_at
FROM deployments WHERE id = ?;

-- name: ListDeployments :many
SELECT id, recipe_digest, profile, placement, fabric, desired_state,
       observed_state, endpoint, model_capabilities, diagnostics, run_id,
       dispatch, created_at, updated_at
FROM deployments ORDER BY created_at DESC;

-- name: ListActiveDeployments :many
SELECT id, recipe_digest, profile, placement, fabric, desired_state,
       observed_state, endpoint, model_capabilities, diagnostics, run_id,
       dispatch, created_at, updated_at
FROM deployments WHERE desired_state = 'running';

-- name: UpdateDeploymentState :exec
UPDATE deployments SET desired_state = ?,
                       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: UpdateDeploymentObserved :exec
UPDATE deployments SET observed_state = ?,
                       endpoint = COALESCE(?, endpoint),
                       model_capabilities = COALESCE(?, model_capabilities),
                       diagnostics = COALESCE(?, diagnostics),
                       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: UpdateDeploymentFabric :exec
UPDATE deployments SET fabric = ? WHERE id = ?;

-- name: UpdateDeploymentRunID :exec
UPDATE deployments SET run_id = ?,
                       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: DeleteDeployment :exec
DELETE FROM deployments WHERE id = ?;

-- name: DeleteDeploymentRuns :exec
DELETE FROM runs WHERE deployment_id = ?;

-- name: CreateRun :exec
INSERT INTO runs (id, module, kind, state, resources, input, deployment_id,
                  legacy_identity)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetRun :one
SELECT id, module, kind, state, resources, input, output, error_code,
       error_message, deployment_id, legacy_identity, created_at, started_at,
       finished_at
FROM runs WHERE id = ?;

-- name: ListRuns :many
SELECT id, module, kind, state, resources, input, output, error_code,
       error_message, deployment_id, legacy_identity, created_at, started_at,
       finished_at
FROM runs
WHERE (:module IS NULL OR module = :module)
  AND (:state IS NULL OR state = :state)
  AND (:created_before IS NULL OR created_at < :created_before)
ORDER BY created_at DESC
LIMIT :limit;

-- name: ListNonTerminalRuns :many
SELECT id, module, kind, state, resources, input, output, error_code,
       error_message, deployment_id, legacy_identity, created_at, started_at,
       finished_at
FROM runs
WHERE state NOT IN ('succeeded', 'failed', 'cancelled', 'interrupted');

-- name: UpdateRunState :exec
UPDATE runs SET state = ?,
                started_at = COALESCE(?, started_at),
                finished_at = COALESCE(?, finished_at),
                error_code = COALESCE(?, error_code),
                error_message = COALESCE(?, error_message)
WHERE id = ?;

-- name: SetRunOutput :exec
UPDATE runs SET output = ? WHERE id = ?;

-- name: GetModuleSettings :one
SELECT module, settings, version, updated_at FROM module_settings WHERE module = ?;

-- name: PutModuleSettings :exec
INSERT INTO module_settings (module, settings, version)
VALUES (?, ?, ?)
ON CONFLICT (module)
DO UPDATE SET settings = excluded.settings,
              version = excluded.version,
              updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE module_settings.version = ?;

-- name: CreateSecret :exec
INSERT INTO secrets (id, name, purpose, nonce, ciphertext)
VALUES (?, ?, ?, ?, ?);

-- name: GetSecret :one
SELECT id, name, purpose, nonce, ciphertext, created_at, updated_at
FROM secrets WHERE id = ?;

-- name: GetSecretByName :one
SELECT id, name, purpose, nonce, ciphertext, created_at, updated_at
FROM secrets WHERE name = ?;

-- name: ListSecrets :many
SELECT id, name, purpose, nonce, ciphertext, created_at, updated_at
FROM secrets ORDER BY name;

-- name: DeleteSecret :exec
DELETE FROM secrets WHERE id = ?;

-- name: UpsertNodeCredential :exec
INSERT INTO node_credentials (node_id, public_key_pem, serial, issued_at, expires_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (node_id)
DO UPDATE SET public_key_pem = excluded.public_key_pem,
              serial = excluded.serial,
              issued_at = excluded.issued_at,
              expires_at = excluded.expires_at;

-- name: GetNodeCredential :one
SELECT node_id, public_key_pem, serial, issued_at, expires_at
FROM node_credentials WHERE node_id = ?;

-- name: CreateTransfer :exec
INSERT INTO transfers (id, artifact_id, source_node, dest_node, dest_path,
                       credential_hash, state)
VALUES (?, ?, ?, ?, ?, ?, 'pending');

-- name: GetTransfer :one
SELECT id, artifact_id, source_node, dest_node, dest_path, credential_hash,
       state, bytes_total, bytes_done, diagnostic, created_at, updated_at
FROM transfers WHERE id = ?;

-- name: ListTransfers :many
SELECT id, artifact_id, source_node, dest_node, dest_path, credential_hash,
       state, bytes_total, bytes_done, diagnostic, created_at, updated_at
FROM transfers ORDER BY created_at DESC;

-- name: UpdateTransferState :exec
UPDATE transfers SET state = ?, diagnostic = ?,
                     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: UpdateTransferProgress :exec
UPDATE transfers SET bytes_done = ?, bytes_total = COALESCE(?, bytes_total),
                     state = 'transferring',
                     updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: InsertBenchmarkResult :exec
INSERT INTO benchmark_results (run_id, language, endpoint, model, requests,
                               successes, prompt_tokens, completion_tokens,
                               total_tokens, wall_seconds, tokens_per_second,
                               latency, first_token, grading, quantization,
                               reasoning, result_path)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListBenchmarkResults :many
SELECT id, run_id, language, endpoint, model, requests, successes, prompt_tokens,
       completion_tokens, total_tokens, wall_seconds, tokens_per_second, latency,
       first_token, grading, quantization, reasoning, result_path, created_at
FROM benchmark_results ORDER BY created_at DESC;

-- name: ListBenchmarkResultsByRun :many
SELECT id, run_id, language, endpoint, model, requests, successes, prompt_tokens,
       completion_tokens, total_tokens, wall_seconds, tokens_per_second, latency,
       first_token, grading, quantization, reasoning, result_path, created_at
FROM benchmark_results WHERE run_id = ? ORDER BY language;

-- name: InsertTelemetry5s :exec
INSERT OR REPLACE INTO telemetry_5s (node_id, ts, payload) VALUES (?, ?, ?);

-- name: InsertTelemetry1m :exec
INSERT OR REPLACE INTO telemetry_1m (node_id, ts, payload) VALUES (?, ?, ?);

-- name: DeleteTelemetry5sOlder :exec
DELETE FROM telemetry_5s WHERE ts < ?;

-- name: DeleteTelemetry1mOlder :exec
DELETE FROM telemetry_1m WHERE ts < ?;

-- name: LatestTelemetry5s :one
SELECT ts, payload FROM telemetry_5s WHERE node_id = ?
ORDER BY ts DESC LIMIT 1;

-- name: AppendEvent :one
INSERT INTO events (type, node_id, payload) VALUES (?, ?, ?)
RETURNING id;

-- name: ListEventsSince :many
SELECT id, ts, type, node_id, payload FROM events
WHERE id > ? ORDER BY id ASC LIMIT ?;

-- name: MaxEventID :one
SELECT COALESCE(MAX(id), 0) FROM events;

-- name: SaveMigrationPlan :exec
INSERT INTO migration_plans (plan_digest, request, plan) VALUES (?, ?, ?);

-- name: GetMigrationPlan :one
SELECT plan_digest, request, plan, created_at
FROM migration_plans WHERE plan_digest = ?;

-- name: AcquireLease :exec
INSERT INTO leases (resource, owner_kind, owner_id, state)
VALUES (?, ?, ?, 'active');

-- name: ReleaseLeases :exec
UPDATE leases SET state = 'released',
                  released_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE owner_kind = ? AND owner_id = ? AND state = 'active';

-- name: ActiveLeaseOwners :many
SELECT owner_kind, owner_id FROM leases
WHERE resource = ? AND state = 'active';

-- name: LeasesForOwner :many
SELECT resource, state FROM leases
WHERE owner_kind = ? AND owner_id = ?
ORDER BY resource;

-- name: UpdateFabricState :exec
UPDATE fabrics SET state = ?, diagnostics = ?,
 updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: CountActiveDeploymentsOnFabric :one
SELECT COUNT(*) FROM deployments
WHERE fabric = ? AND desired_state = 'running';

-- name: SetDeploymentDispatch :exec
UPDATE deployments SET dispatch = ?,
 updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: ActiveLeases :many
SELECT resource FROM leases WHERE state = 'active';
