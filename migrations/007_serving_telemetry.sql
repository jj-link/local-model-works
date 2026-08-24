-- Persist resolved endpoint model identity so model classification survives
-- restart without re-parsing recipes on every serving poll. Nullable: existing
-- deployments fall back to probing /v1/models or report "--".
ALTER TABLE deployments ADD COLUMN endpoint_model TEXT;
ALTER TABLE deployments ADD COLUMN endpoint_path TEXT;

-- Serving telemetry mirrors node telemetry: five-second raw rows and one-minute
-- aggregates, bounded by the telemetry retention policy.
CREATE TABLE IF NOT EXISTS serving_telemetry_5s (
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    ts            INTEGER NOT NULL,
    payload       TEXT NOT NULL,
    PRIMARY KEY (deployment_id, ts)
);

CREATE TABLE IF NOT EXISTS serving_telemetry_1m (
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    ts            INTEGER NOT NULL,
    payload       TEXT NOT NULL,
    PRIMARY KEY (deployment_id, ts)
);
