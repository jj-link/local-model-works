-- name: CreateCodingTrace :exec
INSERT INTO coding_traces (
    id, run_id, experiment_id, task_id, problem, repository, base_revision,
    model_source, model, scaffold, sampling, state, schema_version,
    redaction_version, retain_until
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'recording', ?, ?, ?);

-- name: GetCodingTrace :one
SELECT * FROM coding_traces WHERE id = ?;

-- name: GetCodingTraceByRunID :one
SELECT * FROM coding_traces WHERE run_id = ?;

-- name: ListCodingTraces :many
SELECT * FROM coding_traces
WHERE (:state IS NULL OR state = :state)
  AND (:task_id IS NULL OR task_id = :task_id)
  AND (:success_label IS NULL OR success_label = :success_label)
  AND (:created_before IS NULL OR created_at < :created_before)
ORDER BY created_at DESC, id DESC
LIMIT :limit;

-- name: ListCodingTracesForExport :many
SELECT * FROM coding_traces
WHERE (:experiment_id IS NULL OR experiment_id = :experiment_id)
  AND state != 'recording'
ORDER BY task_id, turn_count, id;

-- name: InsertCodingTraceEvent :exec
INSERT INTO coding_trace_events (
    trace_id, sequence, event_id, agent_id, parent_agent_id, occurred_at,
    kind, payload, input_tokens, output_tokens, redaction_count
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetCodingTraceEvent :one
SELECT * FROM coding_trace_events WHERE trace_id = ? AND sequence = ?;

-- name: ListCodingTraceEvents :many
SELECT * FROM coding_trace_events
WHERE trace_id = ? AND sequence >= ?
ORDER BY sequence
LIMIT ?;
-- name: ListAllCodingTraceEvents :many
SELECT * FROM coding_trace_events
WHERE trace_id = ?
ORDER BY sequence;


-- name: GetCodingTraceStream :one
SELECT * FROM coding_trace_streams
WHERE trace_id = ? AND node_id = ? AND rank = ? AND source = ?;

-- name: CreateCodingTraceStream :exec
INSERT INTO coding_trace_streams (
    trace_id, node_id, rank, source, byte_offset, next_event_sequence
) VALUES (?, ?, ?, ?, 0, 0);

-- name: AdvanceCodingTraceStream :execrows
UPDATE coding_trace_streams
SET byte_offset = ?, next_event_sequence = ?, final_offset = ?,
    eof_acknowledged = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE trace_id = ? AND node_id = ? AND rank = ? AND source = ?
  AND byte_offset = ? AND next_event_sequence = ?;

-- name: CompleteCodingTrace :execrows
UPDATE coding_traces
SET state = 'completed', final_diff = ?, verification_id = ?, success_label = ?,
    failure_kind = ?, token_count = ?, turn_count = ?, redaction_count = ?,
    digest = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND state = 'recording';

-- name: InterruptCodingTrace :execrows
UPDATE coding_traces
SET state = 'interrupted', failure_kind = ?, token_count = ?, turn_count = ?,
    redaction_count = ?, digest = ?,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND state = 'recording';

-- name: CreateCodingTraceVerification :exec
INSERT INTO coding_trace_verifications (
    id, trace_id, command, timeout_seconds, exit_status, stdout, stderr,
    fail_to_pass_report, pass_to_pass_report, status, failure_kind
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetCodingTraceVerification :one
SELECT * FROM coding_trace_verifications WHERE trace_id = ?;

-- name: SetCodingTracePinned :execrows
UPDATE coding_traces
SET pinned = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: DeleteExpiredCodingTraces :execrows
DELETE FROM coding_traces
WHERE pinned = 0 AND state != 'recording' AND retain_until IS NOT NULL AND retain_until < ?;

-- name: DeleteCodingTrace :execrows
DELETE FROM coding_traces WHERE id = ? AND state != 'recording';

-- name: CreateSweGymExperiment :exec
INSERT INTO swe_gym_experiments (
    id, state, config, config_digest, plan, plan_digest, manifest, total_items
) VALUES (?, 'planned', ?, ?, ?, ?, ?, ?);

-- name: GetSweGymExperiment :one
SELECT * FROM swe_gym_experiments WHERE id = ?;

-- name: ListSweGymExperiments :many
SELECT * FROM swe_gym_experiments
WHERE (:created_before IS NULL OR created_at < :created_before)
ORDER BY created_at DESC, id DESC LIMIT :limit;

-- name: SetSweGymExperimentRun :execrows
UPDATE swe_gym_experiments
SET run_id = ?, state = 'queued', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND state = 'planned' AND plan_digest = ?;

-- name: UpdateSweGymExperimentState :execrows
UPDATE swe_gym_experiments
SET state = sqlc.arg(state), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id);
-- name: FinishSweGymExperiment :execrows
UPDATE swe_gym_experiments
SET state = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;


-- name: CreateSweGymWorkItem :exec
INSERT INTO swe_gym_work_items (id, experiment_id, task_id, rollout_index, state)
VALUES (?, ?, ?, ?, 'queued');

-- name: ListSweGymWorkItems :many
SELECT * FROM swe_gym_work_items WHERE experiment_id = ? ORDER BY task_id, rollout_index;

-- name: ClaimSweGymWorkItem :execrows
UPDATE swe_gym_work_items
SET state = 'running', attempt = attempt + 1, child_run_id = ?, node_id = ?,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND state IN ('queued','infrastructure_error');

-- name: FinishSweGymWorkItem :execrows
UPDATE swe_gym_work_items
SET state = ?, trace_id = ?, output = ?, error_code = ?, error_message = ?,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: SetSweGymWorkItemQueued :execrows
UPDATE swe_gym_work_items
SET state = 'queued', child_run_id = NULL, node_id = NULL, error_code = NULL,
    error_message = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    finished_at = NULL
WHERE id = ? AND state IN ('cancelled','infrastructure_error');

-- name: RecountSweGymExperiment :exec
UPDATE swe_gym_experiments SET
    completed_items = (SELECT COUNT(*) FROM swe_gym_work_items wi WHERE wi.experiment_id = sqlc.arg(experiment_id) AND wi.state IN ('resolved','unresolved','infrastructure_error','cancelled')),
    resolved_items = (SELECT COUNT(*) FROM swe_gym_work_items wi WHERE wi.experiment_id = sqlc.arg(experiment_id) AND wi.state = 'resolved'),
    unresolved_items = (SELECT COUNT(*) FROM swe_gym_work_items wi WHERE wi.experiment_id = sqlc.arg(experiment_id) AND wi.state = 'unresolved'),
    infrastructure_errors = (SELECT COUNT(*) FROM swe_gym_work_items wi WHERE wi.experiment_id = sqlc.arg(experiment_id) AND wi.state = 'infrastructure_error'),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(experiment_id);

-- name: CreateCodingTraceExport :exec
INSERT INTO coding_trace_exports (id, run_id, state, selection, seed)
VALUES (?, ?, 'queued', ?, ?);

-- name: GetCodingTraceExport :one
SELECT * FROM coding_trace_exports WHERE id = ?;

-- name: ListCodingTraceExports :many
SELECT * FROM coding_trace_exports ORDER BY created_at DESC, id DESC LIMIT ?;

-- name: UpdateCodingTraceExport :execrows
UPDATE coding_trace_exports
SET state = sqlc.arg(state), artifact_path = sqlc.arg(artifact_path), manifest_digest = sqlc.arg(manifest_digest), canonical_count = sqlc.arg(canonical_count),
    policy_count = sqlc.arg(policy_count), verifier_count = sqlc.arg(verifier_count), excluded_count = sqlc.arg(excluded_count),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id);
-- name: FinishCodingTraceExport :execrows
UPDATE coding_trace_exports
SET state = ?, artifact_path = ?, manifest_digest = ?, canonical_count = ?,
    policy_count = ?, verifier_count = ?, excluded_count = ?,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;
