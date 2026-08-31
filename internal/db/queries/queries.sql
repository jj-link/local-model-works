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

-- name: CreateBrowserLoginToken :exec
INSERT INTO browser_login_tokens (token_hash, username, created_at, expires_at)
VALUES (?, ?, ?, ?);

-- name: ConsumeBrowserLoginToken :one
DELETE FROM browser_login_tokens
WHERE token_hash = ? AND expires_at > ?
RETURNING username;

-- name: DeleteExpiredBrowserLoginTokens :exec
DELETE FROM browser_login_tokens WHERE expires_at <= ?;

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
                     members, bindings, state, diagnostics, version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'incomplete', '[]', ?);

-- name: GetFabric :one
SELECT * FROM fabrics WHERE id = ?;

-- name: GetFabricByIfMatch :one
SELECT * FROM fabrics WHERE id = ? AND version = ?;

-- name: ListFabrics :many
SELECT * FROM fabrics ORDER BY name;

-- name: UpdateFabric :exec
UPDATE fabrics SET transport = ?, interface_name = ?, address = ?, rdma_device = ?,
                   members = ?, bindings = ?, state = ?, diagnostics = ?, version = ?,
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
INSERT OR IGNORE INTO recipes (digest, name, version, description, license, source, manifest)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetRecipe :one
SELECT digest, name, version, description, license, source, manifest, installed_at
FROM recipes WHERE digest = ?;

-- name: ListRecipes :many
SELECT digest, name, version, description, license, source, manifest, installed_at
FROM recipes ORDER BY installed_at DESC;
-- name: ListUnlinkedRecipes :many
SELECT r.digest, r.name, r.version, r.description, r.license, r.source, r.manifest, r.installed_at
FROM recipes r
WHERE NOT EXISTS (
    SELECT 1 FROM recipe_repository_versions v WHERE v.recipe_digest = r.digest
)
ORDER BY r.installed_at DESC, r.digest DESC;


-- name: ListRecipeRepositories :many
SELECT id, source_url, source_path, tracking_ref, current_digest,
       observed_head_commit, observed_head_tree, head_checked_at,
       created_at, updated_at
FROM recipe_repositories
ORDER BY updated_at DESC, id;

-- name: GetRecipeRepository :one
SELECT id, source_url, source_path, tracking_ref, current_digest,
       observed_head_commit, observed_head_tree, head_checked_at,
       created_at, updated_at
FROM recipe_repositories
WHERE id = ?;

-- name: ListRecipeRepositoryVersions :many
SELECT v.repository_id, v.recipe_digest, v.commit_sha, v.tree_sha,
       v.canonical, v.installed_at,
       r.name, r.version, r.description, r.license, r.source, r.manifest
FROM recipe_repository_versions v
JOIN recipes r ON r.digest = v.recipe_digest
WHERE v.repository_id = ?
ORDER BY v.installed_at DESC, v.recipe_digest DESC;

-- name: GetRecipeRepositoryVersionByDigest :one
SELECT repository_id, recipe_digest, commit_sha, tree_sha, canonical, installed_at
FROM recipe_repository_versions
WHERE recipe_digest = ?;

-- ---------------------------------------------------------------- launch profiles

-- name: CreateLaunchProfile :exec
INSERT INTO launch_profiles (id, name, recipe_digest, variants, parameters)
VALUES (?, ?, ?, ?, ?);

-- name: ListLaunchProfilesByRecipe :many
SELECT id, name, recipe_digest, variants, parameters, created_at, updated_at
FROM launch_profiles WHERE recipe_digest = ? ORDER BY created_at DESC, name;

-- name: GetLaunchProfile :one
SELECT id, name, recipe_digest, variants, parameters, created_at, updated_at
FROM launch_profiles WHERE id = ?;

-- name: GetLaunchProfileByName :one
SELECT id, name, recipe_digest, variants, parameters, created_at, updated_at
FROM launch_profiles WHERE recipe_digest = ? AND name = ?;

-- name: UpdateLaunchProfile :exec
UPDATE launch_profiles
SET name = ?, variants = ?, parameters = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: DeleteLaunchProfile :exec
DELETE FROM launch_profiles WHERE id = ?;

-- name: DeleteLaunchProfilesByDigestAndName :exec
DELETE FROM launch_profiles WHERE recipe_digest = ? AND name = ?;

-- name: UpsertRecipeRepository :exec
INSERT INTO recipe_repositories (
    id, source_url, source_path, tracking_ref, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(source_url, source_path) DO UPDATE SET
    tracking_ref = excluded.tracking_ref,
    updated_at = excluded.updated_at;

-- name: AttachRecipeRepositoryVersion :exec
INSERT INTO recipe_repository_versions (
    repository_id, recipe_digest, commit_sha, tree_sha, canonical, installed_at
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(repository_id, recipe_digest) DO UPDATE SET
    commit_sha = excluded.commit_sha,
    tree_sha = excluded.tree_sha,
    canonical = excluded.canonical,
    installed_at = excluded.installed_at;
-- name: ClearCanonicalRecipeRepositoryCommit :exec
UPDATE recipe_repository_versions
SET canonical = 0
WHERE repository_id = ? AND commit_sha = ? AND canonical = 1;


-- name: SetRecipeRepositoryCurrent :exec
UPDATE recipe_repositories
SET current_digest = ?, updated_at = ?
WHERE id = ?;

-- name: SetRecipeRepositoryHead :exec
UPDATE recipe_repositories
SET tracking_ref = ?, observed_head_commit = ?, observed_head_tree = ?,
    head_checked_at = ?, updated_at = ?
WHERE id = ?;


-- name: DeleteRecipe :exec
DELETE FROM recipes WHERE digest = ?;

-- name: DeleteRecipeRepositoryVersionByDigest :exec
DELETE FROM recipe_repository_versions WHERE recipe_digest = ?;

-- name: SetRecipeRepositoryVersionCanonical :exec
UPDATE recipe_repository_versions
SET canonical = 1
WHERE repository_id = ? AND recipe_digest = ?;

-- name: DeleteRecipeRepository :exec
DELETE FROM recipe_repositories WHERE id = ?;

-- name: RecipeReferencedByDeployments :one
SELECT COUNT(*) FROM deployments WHERE recipe_digest = ?;

-- name: RecipeReferencedByRuns :one
SELECT COUNT(*) FROM runs r
JOIN deployments d ON d.id = r.deployment_id
WHERE d.recipe_digest = ?;

-- name: CreateDeployment :exec
INSERT INTO deployments (id, recipe_digest, parameters, placement, fabric,
                         desired_state, observed_state, run_id)
VALUES (?, ?, ?, ?, ?, 'running', 'unknown', ?);

-- name: GetDeployment :one
SELECT id, recipe_digest, parameters, placement, fabric, desired_state,
       observed_state, endpoint, endpoint_model, endpoint_path,
       model_capabilities, diagnostics, run_id,
       dispatch, created_at, updated_at
FROM deployments WHERE id = ?;

-- name: ListDeployments :many
SELECT id, recipe_digest, parameters, placement, fabric, desired_state,
       observed_state, endpoint, endpoint_model, endpoint_path,
       model_capabilities, diagnostics, run_id,
       dispatch, created_at, updated_at
FROM deployments ORDER BY created_at DESC;
-- name: ListDeploymentMonitorTargets :many
SELECT id, desired_state, observed_state, endpoint, endpoint_model, endpoint_path
FROM deployments
WHERE desired_state = 'running'
  AND observed_state IN ('healthy', 'degraded')
  AND endpoint IS NOT NULL AND endpoint != '';

-- name: GetDeploymentByRecipeParametersPlacement :one
SELECT id, recipe_digest, parameters, placement, fabric, desired_state,
       observed_state, endpoint, endpoint_model, endpoint_path,
       model_capabilities, diagnostics, run_id,
       dispatch, created_at, updated_at
FROM deployments
WHERE recipe_digest = ? AND parameters = ? AND placement = ?;

-- name: ListActiveDeployments :many
SELECT id, recipe_digest, parameters, placement, fabric, desired_state,
       observed_state, endpoint, endpoint_model, endpoint_path,
       model_capabilities, diagnostics, run_id,
       dispatch, created_at, updated_at
FROM deployments WHERE desired_state = 'running';
-- name: ListRepositoryActiveDeployments :many
SELECT d.id, d.recipe_digest, d.parameters, d.placement, d.fabric,
       d.desired_state, d.observed_state, d.endpoint, d.endpoint_model,
       d.endpoint_path, d.model_capabilities, d.diagnostics, d.run_id,
       d.dispatch, d.created_at, d.updated_at
FROM deployments d
WHERE d.desired_state = 'running'
  AND EXISTS (
      SELECT 1
      FROM recipe_repository_versions v
      WHERE v.recipe_digest = d.recipe_digest
        AND v.repository_id = ?
  )
ORDER BY d.id;


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

-- name: SetDeploymentStopping :exec
UPDATE deployments SET desired_state = 'stopped',
                       observed_state = 'stopping',
                       endpoint = NULL,
                       diagnostics = ?,
                       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: RestartDeployment :exec
UPDATE deployments SET run_id = ?,
                       placement = ?,
                       fabric = ?,
                       endpoint = ?,
                       endpoint_model = ?,
                       endpoint_path = ?,
                       desired_state = 'running',
                       observed_state = 'unknown',
                       dispatch = '{}',
                       diagnostics = '[]',
                       model_capabilities = NULL,
                       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: ClearStoppedDeploymentEndpoint :exec
UPDATE deployments SET endpoint = NULL,
                       diagnostics = ?,
                       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND desired_state = 'stopped' AND observed_state = 'stopped';

-- name: UpdateDeploymentFabric :exec
UPDATE deployments SET fabric = ? WHERE id = ?;

-- name: UpdateDeploymentPlacement :exec
UPDATE deployments SET placement = ?,
                       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

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
       finished_at, progress
FROM runs WHERE id = ?;

-- name: ListRuns :many
SELECT id, module, kind, state, resources, input, output, error_code,
       error_message, deployment_id, legacy_identity, created_at, started_at,
       finished_at, progress
FROM runs
WHERE (:module IS NULL OR module = :module)
  AND (:state IS NULL OR state = :state)
  AND (:created_before IS NULL OR created_at < :created_before)
ORDER BY created_at DESC
LIMIT :limit;

-- name: ListNonTerminalRuns :many
SELECT id, module, kind, state, resources, input, output, error_code,
       error_message, deployment_id, legacy_identity, created_at, started_at,
       finished_at, progress
FROM runs
WHERE state NOT IN ('succeeded', 'failed', 'cancelled', 'interrupted');

-- name: ListRecipeUpdateRuns :many
SELECT id, module, kind, state, resources, input, output, error_code,
       error_message, deployment_id, legacy_identity, created_at, started_at,
       finished_at, progress
FROM runs
WHERE kind = 'recipe-update'
  AND state NOT IN ('succeeded', 'failed', 'cancelled', 'interrupted')
ORDER BY created_at, id;

-- name: UpdateRunState :exec
UPDATE runs SET state = ?,
                started_at = COALESCE(?, started_at),
                finished_at = COALESCE(?, finished_at),
                error_code = COALESCE(?, error_code),
                error_message = COALESCE(?, error_message)
WHERE id = ?;

-- name: SetRunOutput :exec
UPDATE runs SET output = ? WHERE id = ?;

-- name: SetRunProgress :exec
UPDATE runs SET progress = ? WHERE id = ?;

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

-- name: ActiveLeasesWithOwners :many
SELECT resource, owner_kind, owner_id
FROM leases
WHERE state = 'active';


-- name: UpdateDeploymentEndpointMetadata :exec
UPDATE deployments SET endpoint_model = ?, endpoint_path = ?,
                       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: LatestTelemetryAll :many
SELECT t.node_id, t.ts, t.payload FROM telemetry_5s t
JOIN (SELECT node_id, MAX(ts) ts FROM telemetry_5s GROUP BY node_id) latest
  ON latest.node_id = t.node_id AND latest.ts = t.ts
ORDER BY t.node_id;

-- name: InsertServingTelemetry5s :exec
INSERT OR REPLACE INTO serving_telemetry_5s (deployment_id, ts, payload) VALUES (?, ?, ?);

-- name: InsertServingTelemetry1m :exec
INSERT OR REPLACE INTO serving_telemetry_1m (deployment_id, ts, payload) VALUES (?, ?, ?);

-- name: DeleteServingTelemetry5sOlder :exec
DELETE FROM serving_telemetry_5s WHERE ts < ?;

-- name: DeleteServingTelemetry1mOlder :exec
DELETE FROM serving_telemetry_1m WHERE ts < ?;

-- name: LatestServingTelemetryAll :many
SELECT s.deployment_id, s.ts, s.payload FROM serving_telemetry_5s s
JOIN (SELECT deployment_id, MAX(ts) ts FROM serving_telemetry_5s GROUP BY deployment_id) latest
  ON latest.deployment_id = s.deployment_id AND latest.ts = s.ts
ORDER BY s.deployment_id;
