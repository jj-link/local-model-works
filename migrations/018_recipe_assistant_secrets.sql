PRAGMA foreign_keys = OFF;

CREATE TABLE secrets_recipe_assistant (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    purpose     TEXT NOT NULL CHECK (purpose IN ('huggingface', 'github', 'registry', 'recipe-assistant')),
    nonce       BLOB NOT NULL,
    ciphertext  BLOB NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

INSERT INTO secrets_recipe_assistant (id, name, purpose, nonce, ciphertext, created_at, updated_at)
SELECT id, name, purpose, nonce, ciphertext, created_at, updated_at
FROM secrets;

DROP TABLE secrets;
ALTER TABLE secrets_recipe_assistant RENAME TO secrets;

PRAGMA foreign_keys = ON;
