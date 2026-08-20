-- Canonical immutable artifact identities include their revision or digest.
-- Merge placements first when an already-canonical row exists.
INSERT OR IGNORE INTO artifact_placements
    (artifact_id, node_id, path, state, verified_at, diagnostics, size_bytes)
SELECT canonical.id, p.node_id, p.path, p.state, p.verified_at, p.diagnostics, p.size_bytes
FROM artifacts legacy
JOIN artifacts canonical ON canonical.identity =
    CASE
      WHEN legacy.identity LIKE 'huggingface://%' THEN 'hf://' || substr(legacy.identity, 15) || '@' || legacy.revision
      WHEN legacy.identity LIKE 'hf://%' THEN legacy.identity || '@' || legacy.revision
      WHEN legacy.identity LIKE 'local://%' AND legacy.digest LIKE 'sha256:%' THEN 'file://' || legacy.digest
    END
JOIN artifact_placements p ON p.artifact_id = legacy.id
WHERE ((legacy.identity LIKE 'huggingface://%' OR (legacy.identity LIKE 'hf://%' AND instr(legacy.identity, '@') = 0))
       AND legacy.revision GLOB '[0-9a-f]*' AND length(legacy.revision) = 40)
   OR (legacy.identity LIKE 'local://%' AND legacy.digest LIKE 'sha256:%' AND length(legacy.digest) = 71);

DELETE FROM artifact_placements
WHERE artifact_id IN (
  SELECT legacy.id FROM artifacts legacy JOIN artifacts canonical ON canonical.identity =
    CASE
      WHEN legacy.identity LIKE 'huggingface://%' THEN 'hf://' || substr(legacy.identity, 15) || '@' || legacy.revision
      WHEN legacy.identity LIKE 'hf://%' THEN legacy.identity || '@' || legacy.revision
      WHEN legacy.identity LIKE 'local://%' AND legacy.digest LIKE 'sha256:%' THEN 'file://' || legacy.digest
    END
  WHERE legacy.id <> canonical.id
);

DELETE FROM artifacts
WHERE id IN (
  SELECT legacy.id FROM artifacts legacy JOIN artifacts canonical ON canonical.identity =
    CASE
      WHEN legacy.identity LIKE 'huggingface://%' THEN 'hf://' || substr(legacy.identity, 15) || '@' || legacy.revision
      WHEN legacy.identity LIKE 'hf://%' THEN legacy.identity || '@' || legacy.revision
      WHEN legacy.identity LIKE 'local://%' AND legacy.digest LIKE 'sha256:%' THEN 'file://' || legacy.digest
    END
  WHERE legacy.id <> canonical.id
);

UPDATE artifacts
SET identity = CASE
  WHEN identity LIKE 'huggingface://%' THEN 'hf://' || substr(identity, 15) || '@' || revision
  WHEN identity LIKE 'hf://%' THEN identity || '@' || revision
  ELSE identity
END
WHERE (identity LIKE 'huggingface://%' OR (identity LIKE 'hf://%' AND instr(identity, '@') = 0))
  AND revision GLOB '[0-9a-f]*' AND length(revision) = 40;

UPDATE artifacts
SET identity = 'file://' || digest
WHERE identity LIKE 'local://%' AND digest LIKE 'sha256:%' AND length(digest) = 71;
