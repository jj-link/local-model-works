CREATE TABLE browser_login_tokens (
    token_hash  TEXT PRIMARY KEY,
    username    TEXT NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);

CREATE INDEX browser_login_tokens_expires_idx
    ON browser_login_tokens(expires_at);
