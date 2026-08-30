UPDATE autoresearch_ideas
SET selected = CASE
    WHEN id = (
        SELECT selected_idea.id
        FROM autoresearch_ideas AS selected_idea
        WHERE selected_idea.project_id = autoresearch_ideas.project_id
          AND selected_idea.selected = 1
        ORDER BY selected_idea.updated_at DESC, selected_idea.id DESC
        LIMIT 1
    ) THEN 1
    ELSE 0
END
WHERE project_id IN (
    SELECT project_id
    FROM autoresearch_ideas
    WHERE selected = 1
    GROUP BY project_id
    HAVING COUNT(*) > 1
);

CREATE TABLE autoresearch_ideas_new (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES autoresearch_projects(id) ON DELETE CASCADE,
    ordinal    INTEGER NOT NULL CHECK (ordinal >= 0),
    source     TEXT NOT NULL CHECK (source IN ('human', 'generated')),
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    selected   INTEGER NOT NULL DEFAULT 0 CHECK (selected IN (0, 1)),
    version    INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE(project_id, ordinal)
);

INSERT INTO autoresearch_ideas_new
    (id, project_id, ordinal, source, title, body, selected, version, created_at, updated_at)
SELECT id,
       project_id,
       CASE WHEN source = 'human' THEN 0 ELSE ordinal END,
       source,
       title,
       body,
       selected,
       version,
       created_at,
       updated_at
FROM autoresearch_ideas;

DROP TABLE autoresearch_ideas;
ALTER TABLE autoresearch_ideas_new RENAME TO autoresearch_ideas;
CREATE INDEX idx_autoresearch_ideas_project_selected
    ON autoresearch_ideas(project_id, selected, ordinal);

ALTER TABLE autoresearch_projects DROP COLUMN runner_node_id;
