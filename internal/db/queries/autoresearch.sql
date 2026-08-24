-- name: CreateAutoResearchProject :exec
INSERT INTO autoresearch_projects (id, name, status, runner_node_id, idea_prompt, config_json)
VALUES (?, ?, ?, ?, ?, ?);

-- name: GetAutoResearchProject :one
SELECT * FROM autoresearch_projects WHERE id = ?;

-- name: ListAutoResearchProjects :many
SELECT * FROM autoresearch_projects ORDER BY updated_at DESC, id;

-- name: UpdateAutoResearchProject :execrows
UPDATE autoresearch_projects
SET name = ?, status = ?, runner_node_id = ?, idea_prompt = ?, config_json = ?,
    version = version + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND version = ?;

-- name: UpdateAutoResearchProjectStatus :execrows
UPDATE autoresearch_projects
SET status = ?, version = version + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND version = ?;

-- name: CreateAutoResearchSource :exec
INSERT INTO autoresearch_sources
(id, project_id, kind, locator, title, metadata_json, local_path, sha256, status, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetAutoResearchSource :one
SELECT * FROM autoresearch_sources WHERE id = ? AND project_id = ?;

-- name: ListAutoResearchSources :many
SELECT * FROM autoresearch_sources WHERE project_id = ? ORDER BY created_at, id;

-- name: UpdateAutoResearchSource :exec
UPDATE autoresearch_sources
SET locator = ?, title = ?, metadata_json = ?, local_path = ?, sha256 = ?, status = ?, error = ?
WHERE id = ? AND project_id = ?;

-- name: CreateAutoResearchIdea :exec
INSERT INTO autoresearch_ideas (id, project_id, ordinal, source, title, body, selected)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetAutoResearchIdea :one
SELECT * FROM autoresearch_ideas WHERE id = ? AND project_id = ?;

-- name: ListAutoResearchIdeas :many
SELECT * FROM autoresearch_ideas WHERE project_id = ? ORDER BY ordinal, id;

-- name: UpdateAutoResearchIdea :execrows
UPDATE autoresearch_ideas
SET title = ?, body = ?, version = version + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND project_id = ? AND version = ?;

-- name: SelectAutoResearchIdea :execrows
UPDATE autoresearch_ideas
SET selected = 1, version = version + 1,
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND project_id = ? AND version = ?;

-- name: CountSelectedAutoResearchIdeas :one
SELECT COUNT(*) FROM autoresearch_ideas WHERE project_id = ? AND selected = 1;

-- name: CreateAutoResearchRun :exec
INSERT INTO autoresearch_runs
(run_id, project_id, factory, parent_run_id, dispatcher_session_id, worker_node_id, config_snapshot)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetAutoResearchRun :one
SELECT * FROM autoresearch_runs WHERE run_id = ?;

-- name: ListAutoResearchRuns :many
SELECT * FROM autoresearch_runs WHERE project_id = ? ORDER BY run_id DESC;

-- name: UpdateAutoResearchRunSession :exec
UPDATE autoresearch_runs SET dispatcher_session_id = ?, worker_node_id = ? WHERE run_id = ?;

-- name: CreateAutoResearchInvocation :exec
INSERT INTO autoresearch_invocations
(id, run_id, parent_id, node_id, role, backend, model, advisor, session_id, state)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetAutoResearchInvocation :one
SELECT * FROM autoresearch_invocations WHERE id = ?;

-- name: ListAutoResearchInvocations :many
SELECT * FROM autoresearch_invocations WHERE run_id = ? ORDER BY started_at, id;

-- name: StartAutoResearchInvocation :execrows
UPDATE autoresearch_invocations
SET state = 'running', session_id = ?, started_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND state = 'queued';

-- name: FinishAutoResearchInvocation :execrows
UPDATE autoresearch_invocations
SET state = ?, input_tokens = ?, output_tokens = ?, cost_usd = ?, error = ?,
    finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND state = 'running';

-- name: CreateAutoResearchMessage :exec
INSERT INTO autoresearch_messages (id, project_id, role, body, changed_paths_json)
VALUES (?, ?, ?, ?, ?);

-- name: ListAutoResearchMessages :many
SELECT * FROM autoresearch_messages WHERE project_id = ? ORDER BY created_at, id;
