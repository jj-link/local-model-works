-- Downloads own attempts, not availability. Existing artifact/package placements
-- and engine observations remain the only sources of file-availability truth.
CREATE TABLE download_items (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id),
    predecessor_item_id TEXT REFERENCES download_items(id),
    resource_key TEXT NOT NULL,
    node_id TEXT NOT NULL REFERENCES nodes(id),
    resource_json TEXT NOT NULL CHECK (
        json_valid(resource_json) AND json_type(resource_json) = 'object'
        AND length(CAST(resource_json AS BLOB)) <= 65536
        AND json_extract(resource_json, '$.key') IS resource_key
        AND json_extract(resource_json, '$.node_id') IS node_id
        AND json_type(resource_json, '$.kind') IS 'text'
        AND json_extract(resource_json, '$.kind') IN ('artifact', 'recipe', 'image')
        AND json_type(resource_json, '$.identity') IS 'text'
        AND length(CAST(json_extract(resource_json, '$.identity') AS BLOB)) BETWEEN 1 AND 2048
        AND json_type(resource_json, '$.destination') IS 'text'
        AND length(CAST(json_extract(resource_json, '$.destination') AS BLOB)) BETWEEN 1 AND 4096
    ),
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN (
        'pending', 'checking', 'transferring', 'verifying', 'cancelling',
        'succeeded', 'failed', 'cancelled', 'interrupted'
    )),
    command_id TEXT,
    transfer_id TEXT,
    checkpoint_json TEXT NOT NULL DEFAULT '{}' CHECK (
        json_valid(checkpoint_json) AND json_type(checkpoint_json) = 'object'
        AND length(CAST(checkpoint_json AS BLOB)) <= 65536
    ),
    error_json TEXT CHECK (error_json IS NULL OR (
        json_valid(error_json) AND json_type(error_json) = 'object'
        AND length(CAST(error_json AS BLOB)) <= 16384
    )),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (run_id, resource_key),
    CHECK (id <> '' AND resource_key <> ''),
    CHECK (command_id IS NULL OR command_id <> ''),
    CHECK (transfer_id IS NULL OR transfer_id <> '')
);
CREATE UNIQUE INDEX idx_download_items_command ON download_items(command_id) WHERE command_id IS NOT NULL;
CREATE UNIQUE INDEX idx_download_items_transfer ON download_items(transfer_id) WHERE transfer_id IS NOT NULL;
CREATE INDEX idx_download_items_node_state ON download_items(node_id, state);
CREATE INDEX idx_download_items_predecessor ON download_items(predecessor_item_id);
CREATE INDEX idx_runs_download_recipe ON runs(json_extract(input, '$.plan.recipe_digest'), created_at DESC, id DESC)
    WHERE module = 'library' AND kind = 'recipe-download';

-- Every write path (including serving/legacy transfers after their cutover)
-- acquires this same lock before dispatch. Destination is the resolved actual
-- path, so different identities cannot write the same destination concurrently.
-- Engine-root destinations serialize pulls on that engine. No lease expires on
-- disconnect, parent-run termination, or controller restart.
CREATE TABLE acquisition_destination_locks (
    node_id TEXT NOT NULL REFERENCES nodes(id),
    destination TEXT NOT NULL,
    identity TEXT NOT NULL,
    platform TEXT NOT NULL DEFAULT '',
    owner_kind TEXT NOT NULL CHECK (owner_kind IN ('download', 'serving', 'transfer')),
    owner_id TEXT NOT NULL,
    run_id TEXT REFERENCES runs(id),
    item_id TEXT REFERENCES download_items(id),
    state TEXT NOT NULL DEFAULT 'held' CHECK (state IN ('held', 'cancelling', 'quiesced')),
    quiesced_at TEXT,
    quiescence_proof TEXT CHECK (quiescence_proof IN ('terminal-ack', 'node-barrier', 'never-dispatched')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (node_id, destination),
    CHECK (destination <> '' AND identity <> '' AND owner_id <> ''),
    CHECK ((owner_kind = 'download' AND item_id IS NOT NULL AND run_id IS NOT NULL AND owner_id = item_id)
        OR (owner_kind <> 'download' AND item_id IS NULL)),
    CHECK ((state = 'quiesced' AND quiesced_at IS NOT NULL AND quiescence_proof IS NOT NULL)
        OR (state <> 'quiesced' AND quiesced_at IS NULL AND quiescence_proof IS NULL))
);
CREATE INDEX idx_acquisition_destination_owner ON acquisition_destination_locks(owner_kind, owner_id);
CREATE INDEX idx_acquisition_destination_run ON acquisition_destination_locks(run_id);

-- Run input is the frozen normalized plan plus credential IDs, never values.
CREATE TRIGGER immutable_download_run_input
BEFORE UPDATE OF input, module, kind ON runs
WHEN OLD.module = 'library' AND OLD.kind = 'recipe-download'
    AND (NEW.input IS NOT OLD.input OR NEW.module IS NOT OLD.module OR NEW.kind IS NOT OLD.kind)
BEGIN
    SELECT RAISE(ABORT, 'download attempt input is immutable');
END;

CREATE TRIGGER download_item_attempt
BEFORE INSERT ON download_items
BEGIN
    SELECT RAISE(ABORT, 'download item requires a recipe-download run')
    WHERE NOT EXISTS (SELECT 1 FROM runs WHERE id = NEW.run_id AND module = 'library' AND kind = 'recipe-download');
    SELECT RAISE(ABORT, 'download predecessor must be terminal with identical content and destination')
    WHERE NEW.predecessor_item_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM download_items AS predecessor
        WHERE predecessor.id = NEW.predecessor_item_id
            AND predecessor.run_id <> NEW.run_id
            AND predecessor.state IN ('succeeded', 'failed', 'cancelled', 'interrupted')
            AND predecessor.resource_key = NEW.resource_key
            AND predecessor.node_id = NEW.node_id
            AND json_extract(predecessor.resource_json, '$.kind') IS json_extract(NEW.resource_json, '$.kind')
            AND json_extract(predecessor.resource_json, '$.identity') IS json_extract(NEW.resource_json, '$.identity')
            AND json_extract(predecessor.resource_json, '$.destination') IS json_extract(NEW.resource_json, '$.destination')
            AND json_extract(predecessor.resource_json, '$.platform') IS json_extract(NEW.resource_json, '$.platform')
            AND json_extract(predecessor.resource_json, '$.index_digest') IS json_extract(NEW.resource_json, '$.index_digest')
            AND json_extract(predecessor.resource_json, '$.manifest_digest') IS json_extract(NEW.resource_json, '$.manifest_digest')
    );
END;

CREATE TRIGGER immutable_download_item
BEFORE UPDATE ON download_items
BEGIN
    SELECT RAISE(ABORT, 'download item identity and attempt ownership are immutable')
    WHERE NEW.id IS NOT OLD.id OR NEW.run_id IS NOT OLD.run_id
        OR NEW.predecessor_item_id IS NOT OLD.predecessor_item_id
        OR NEW.resource_key IS NOT OLD.resource_key OR NEW.node_id IS NOT OLD.node_id
        OR NEW.resource_json IS NOT OLD.resource_json
        OR (OLD.command_id IS NOT NULL AND NEW.command_id IS NOT OLD.command_id)
        OR (OLD.transfer_id IS NOT NULL AND NEW.transfer_id IS NOT OLD.transfer_id);
    SELECT RAISE(ABORT, 'terminal download item cannot be updated')
    WHERE OLD.state IN ('succeeded', 'failed', 'cancelled', 'interrupted');
    SELECT RAISE(ABORT, 'invalid download item state transition')
    WHERE NEW.state <> OLD.state AND NOT (
        (OLD.state = 'pending' AND NEW.state IN ('checking', 'cancelling', 'failed', 'interrupted'))
        OR (OLD.state = 'checking' AND NEW.state IN ('transferring', 'verifying', 'succeeded', 'cancelling', 'failed', 'interrupted'))
        OR (OLD.state = 'transferring' AND NEW.state IN ('verifying', 'cancelling', 'failed', 'interrupted'))
        OR (OLD.state = 'verifying' AND NEW.state IN ('succeeded', 'cancelling', 'failed', 'interrupted'))
        OR (OLD.state = 'cancelling' AND NEW.state IN ('cancelled', 'interrupted'))
    );
END;

CREATE TRIGGER download_destination_owner
BEFORE INSERT ON acquisition_destination_locks
WHEN NEW.owner_kind = 'download'
BEGIN
    SELECT RAISE(ABORT, 'destination lock must match its active download item')
    WHERE NOT EXISTS (
        SELECT 1 FROM download_items WHERE id = NEW.item_id AND run_id = NEW.run_id
            AND node_id = NEW.node_id AND state IN ('pending', 'checking', 'transferring', 'verifying')
            AND json_extract(resource_json, '$.identity') IS NEW.identity
            AND json_extract(resource_json, '$.destination') IS NEW.destination
            AND COALESCE(json_extract(resource_json, '$.platform'), '') = NEW.platform
    );
END;

CREATE TRIGGER immutable_acquisition_destination_owner
BEFORE UPDATE ON acquisition_destination_locks
BEGIN
    SELECT RAISE(ABORT, 'destination ownership is immutable until quiescence and release')
    WHERE NEW.node_id IS NOT OLD.node_id OR NEW.destination IS NOT OLD.destination
        OR NEW.identity IS NOT OLD.identity OR NEW.platform IS NOT OLD.platform
        OR NEW.owner_kind IS NOT OLD.owner_kind OR NEW.owner_id IS NOT OLD.owner_id
        OR NEW.run_id IS NOT OLD.run_id OR NEW.item_id IS NOT OLD.item_id;
    SELECT RAISE(ABORT, 'destination lock cannot resume a quiesced or cancelling writer')
    WHERE OLD.state = 'quiesced' OR (OLD.state = 'cancelling' AND NEW.state = 'held');
END;

CREATE TRIGGER acquisition_destination_release_barrier
BEFORE DELETE ON acquisition_destination_locks
WHEN OLD.state <> 'quiesced'
BEGIN
    SELECT RAISE(ABORT, 'destination writer must be acknowledged quiescent before release');
END;
