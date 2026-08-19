-- Per-rank dispatch phase tracking on deployments. The value is JSON:
-- {"<rank>": "<phase>"} where phase is the last successfully completed
-- step of the workload sequence: none -> pulled -> created -> started.
-- A server restart resumes dispatch from the persisted phase; the
-- partial unique index on active leases (003) still owns exclusivity.
ALTER TABLE deployments ADD COLUMN dispatch TEXT NOT NULL DEFAULT '{}';
