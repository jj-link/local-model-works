CREATE TABLE IF NOT EXISTS benchmark_run_results (
    run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    benchmark_id TEXT NOT NULL,
    benchmark_version TEXT NOT NULL,
    harness TEXT NOT NULL,
    generation_deployment_id TEXT REFERENCES deployments(id) ON DELETE SET NULL,
    verification_deployment_id TEXT REFERENCES deployments(id) ON DELETE SET NULL,
    origin_autoresearch_run_id TEXT,
    execution_node_id TEXT REFERENCES nodes(id) ON DELETE SET NULL,
    task_count INTEGER NOT NULL CHECK (task_count >= 0),
    candidate_count INTEGER NOT NULL CHECK (candidate_count BETWEEN 1 AND 10),
    passed_count INTEGER NOT NULL DEFAULT 0 CHECK (passed_count >= 0),
    pass_at_1 REAL CHECK (pass_at_1 IS NULL OR pass_at_1 BETWEEN 0 AND 1),
    verifier_pass_rate REAL CHECK (verifier_pass_rate IS NULL OR verifier_pass_rate BETWEEN 0 AND 1),
    oracle_pass_rate REAL CHECK (oracle_pass_rate IS NULL OR oracle_pass_rate BETWEEN 0 AND 1),
    prompt_tokens INTEGER NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    total_tokens INTEGER NOT NULL DEFAULT 0 CHECK (total_tokens >= 0),
    wall_seconds REAL NOT NULL DEFAULT 0 CHECK (wall_seconds >= 0),
    summary_artifact_id TEXT REFERENCES artifacts(id) ON DELETE SET NULL,
    bundle_artifact_id TEXT REFERENCES artifacts(id) ON DELETE SET NULL,
    metrics_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metrics_json)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE TABLE IF NOT EXISTS benchmark_trial_results (
    run_id TEXT NOT NULL REFERENCES benchmark_run_results(run_id) ON DELETE CASCADE,
    task_id TEXT NOT NULL,
    candidate_index INTEGER NOT NULL CHECK (candidate_index >= 0),
    official_pass INTEGER NOT NULL CHECK (official_pass IN (0, 1)),
    verifier_selected INTEGER NOT NULL DEFAULT 0 CHECK (verifier_selected IN (0, 1)),
    verifier_score REAL,
    trajectory_path TEXT NOT NULL,
    metrics_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metrics_json)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (run_id, task_id, candidate_index)
);

INSERT INTO benchmark_run_results (
    run_id, benchmark_id, benchmark_version, harness,
    generation_deployment_id, task_count, candidate_count, passed_count,
    pass_at_1, prompt_tokens, completion_tokens, total_tokens, wall_seconds,
    metrics_json, created_at
)
SELECT results.run_id, 'lmw-code-generation', '1', 'lmw-oneshot',
       CASE WHEN EXISTS (SELECT 1 FROM deployments WHERE id=json_extract(run.input,'$.deployment_id'))
            THEN json_extract(run.input,'$.deployment_id') ELSE NULL END,
       COUNT(*), 1, SUM(CASE WHEN results.successes>0 THEN 1 ELSE 0 END),
       CASE WHEN SUM(results.requests)>0 THEN CAST(SUM(results.successes) AS REAL)/SUM(results.requests) ELSE NULL END,
       SUM(results.prompt_tokens), SUM(results.completion_tokens), SUM(results.total_tokens), SUM(results.wall_seconds),
       json_object('backfilled',json('true')), MIN(results.created_at)
FROM benchmark_results AS results JOIN runs AS run ON run.id=results.run_id
GROUP BY results.run_id
ON CONFLICT(run_id) DO NOTHING;

-- Rebuild both aggregate tables on every baseline so a deployed donor table's
-- obsolete research column and foreign key are removed without dropping rows.
-- Preserve the old research linkage as inert historical metadata before the
-- aggregate table is rebuilt without a cross-product foreign key.
UPDATE runs
SET input = json_set(input, '$.origin.legacy_autoresearch_run_id', (
    SELECT origin_autoresearch_run_id
    FROM benchmark_run_results
    WHERE benchmark_run_results.run_id = runs.id
))
WHERE id IN (
    SELECT run_id FROM benchmark_run_results
    WHERE origin_autoresearch_run_id IS NOT NULL
);

CREATE TABLE benchmark_run_results_next (
    run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    benchmark_id TEXT NOT NULL, benchmark_version TEXT NOT NULL, harness TEXT NOT NULL,
    generation_deployment_id TEXT REFERENCES deployments(id) ON DELETE SET NULL,
    verification_deployment_id TEXT REFERENCES deployments(id) ON DELETE SET NULL,
    execution_node_id TEXT REFERENCES nodes(id) ON DELETE SET NULL,
    task_count INTEGER NOT NULL CHECK(task_count>=0),
    candidate_count INTEGER NOT NULL CHECK(candidate_count BETWEEN 1 AND 10),
    passed_count INTEGER NOT NULL DEFAULT 0 CHECK(passed_count>=0),
    pass_at_1 REAL CHECK(pass_at_1 IS NULL OR pass_at_1 BETWEEN 0 AND 1),
    verifier_pass_rate REAL CHECK(verifier_pass_rate IS NULL OR verifier_pass_rate BETWEEN 0 AND 1),
    oracle_pass_rate REAL CHECK(oracle_pass_rate IS NULL OR oracle_pass_rate BETWEEN 0 AND 1),
    prompt_tokens INTEGER NOT NULL DEFAULT 0 CHECK(prompt_tokens>=0),
    completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK(completion_tokens>=0),
    total_tokens INTEGER NOT NULL DEFAULT 0 CHECK(total_tokens>=0),
    wall_seconds REAL NOT NULL DEFAULT 0 CHECK(wall_seconds>=0),
    summary_artifact_id TEXT REFERENCES artifacts(id) ON DELETE SET NULL,
    bundle_artifact_id TEXT REFERENCES artifacts(id) ON DELETE SET NULL,
    metrics_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(metrics_json)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
INSERT INTO benchmark_run_results_next (
 run_id,benchmark_id,benchmark_version,harness,generation_deployment_id,
 verification_deployment_id,execution_node_id,task_count,candidate_count,
 passed_count,pass_at_1,verifier_pass_rate,oracle_pass_rate,prompt_tokens,
 completion_tokens,total_tokens,wall_seconds,summary_artifact_id,
 bundle_artifact_id,metrics_json,created_at
)
SELECT run_id,benchmark_id,benchmark_version,harness,generation_deployment_id,
 verification_deployment_id,execution_node_id,task_count,candidate_count,
 passed_count,pass_at_1,verifier_pass_rate,oracle_pass_rate,prompt_tokens,
 completion_tokens,total_tokens,wall_seconds,summary_artifact_id,
 bundle_artifact_id,metrics_json,created_at
FROM benchmark_run_results;
CREATE TABLE benchmark_trial_results_next (
    run_id TEXT NOT NULL REFERENCES benchmark_run_results_next(run_id) ON DELETE CASCADE,
    task_id TEXT NOT NULL, candidate_index INTEGER NOT NULL CHECK(candidate_index>=0),
    official_pass INTEGER NOT NULL CHECK(official_pass IN(0,1)),
    verifier_selected INTEGER NOT NULL DEFAULT 0 CHECK(verifier_selected IN(0,1)),
    verifier_score REAL, trajectory_path TEXT NOT NULL,
    metrics_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(metrics_json)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY(run_id,task_id,candidate_index)
);
INSERT INTO benchmark_trial_results_next SELECT * FROM benchmark_trial_results;
DROP TABLE benchmark_trial_results;
DROP TABLE benchmark_run_results;
ALTER TABLE benchmark_run_results_next RENAME TO benchmark_run_results;
ALTER TABLE benchmark_trial_results_next RENAME TO benchmark_trial_results;
CREATE INDEX idx_benchmark_run_results_created ON benchmark_run_results(created_at DESC,run_id DESC);
CREATE INDEX idx_benchmark_trial_results_run_task ON benchmark_trial_results(run_id,task_id,candidate_index);

CREATE TABLE api_tokens (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    token_hash TEXT NOT NULL UNIQUE,
    scopes_json TEXT NOT NULL CHECK(json_valid(scopes_json)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    revoked_at TEXT
);
CREATE INDEX idx_runs_benchmark_origin
ON runs(
  json_extract(input,'$.origin.client_id'),
  json_extract(input,'$.origin.run_id'),
  json_extract(input,'$.origin.project_id'),
  created_at DESC,id DESC
) WHERE module='benchmarks';
