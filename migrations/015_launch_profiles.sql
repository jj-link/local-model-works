CREATE TABLE launch_profiles (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    recipe_digest TEXT NOT NULL REFERENCES recipes(digest) ON DELETE RESTRICT,
    variants     TEXT NOT NULL DEFAULT '{}',
    parameters   TEXT NOT NULL DEFAULT '{}',
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (recipe_digest, name)
);
CREATE INDEX idx_launch_profiles_recipe ON launch_profiles(recipe_digest);
