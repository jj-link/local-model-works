-- Deployment rows store the resolved launch settings (parameter overrides
-- validated against the recipe manifest) instead of a recipe-authored
-- profile name; profiles move to operator-owned launch_profiles. The
-- legacy name values are dropped: saved launch profiles replace them.
-- Rebuilding the parent table fires runs.deployment_id ON DELETE SET NULL
-- while foreign keys are enabled. Preserve those references explicitly.
CREATE TEMP TABLE deployment_run_refs AS
SELECT id AS run_id, deployment_id
FROM runs
WHERE deployment_id IS NOT NULL;

CREATE TABLE deployments_new (
    id                TEXT PRIMARY KEY,
    recipe_digest     TEXT NOT NULL REFERENCES recipes(digest) ON DELETE RESTRICT,
    parameters        TEXT NOT NULL DEFAULT (char(123,125)),
    placement         TEXT NOT NULL,
    fabric            TEXT,
    desired_state     TEXT NOT NULL DEFAULT 'running' CHECK (desired_state IN ('running', 'stopped')),
    observed_state    TEXT NOT NULL DEFAULT 'unknown',
    endpoint          TEXT,
    model_capabilities TEXT,
    diagnostics       TEXT NOT NULL DEFAULT '[]',
    run_id            TEXT,
    dispatch          TEXT NOT NULL DEFAULT '{}',
    endpoint_model    TEXT,
    endpoint_path     TEXT,
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (recipe_digest, parameters, placement)
);
INSERT INTO deployments_new (id, recipe_digest, parameters, placement, fabric,
                             desired_state, observed_state, endpoint,
                             model_capabilities, diagnostics, run_id,
                             dispatch, endpoint_model, endpoint_path,
                             created_at, updated_at)
SELECT id, recipe_digest, char(123,125), placement, fabric, desired_state,
       observed_state, endpoint, model_capabilities, diagnostics, run_id,
       dispatch, endpoint_model, endpoint_path, created_at, updated_at
FROM deployments;
DROP TABLE deployments;
ALTER TABLE deployments_new RENAME TO deployments;
UPDATE runs
SET deployment_id = (
    SELECT deployment_id
    FROM deployment_run_refs
    WHERE deployment_run_refs.run_id = runs.id
)
WHERE id IN (SELECT run_id FROM deployment_run_refs);
DROP TABLE deployment_run_refs;
CREATE INDEX IF NOT EXISTS idx_deployments_state ON deployments(desired_state, observed_state);
