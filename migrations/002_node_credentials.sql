-- Node credentials: the agent-generated public key and issued certificate
-- metadata. Rotation re-signs the same public key with a new serial.
CREATE TABLE IF NOT EXISTS node_credentials (
    node_id        TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    public_key_pem TEXT NOT NULL,
    serial         TEXT NOT NULL,
    issued_at      TEXT NOT NULL,
    expires_at     TEXT NOT NULL
);
