ALTER TABLE import_jobs DROP COLUMN IF EXISTS replies_checked_at;

-- Back to the bare ts where it names one message: a ts two conversations' messages
-- share keeps the conversation's key (the code before this finds neither, as it
-- found only one of them before).
WITH lone AS (
    SELECT provider, source_workspace_name, entity_type, split_part(source_id, '/', 2) AS ts
    FROM import_workspace_id_map
    WHERE provider = 'slack' AND entity_type IN ('message', 'comment') AND position('/' in source_id) > 0
    GROUP BY provider, source_workspace_name, entity_type, split_part(source_id, '/', 2)
    HAVING count(*) = 1
)
UPDATE import_workspace_id_map w
SET source_id = l.ts
FROM lone l
WHERE w.provider = l.provider AND w.source_workspace_name = l.source_workspace_name
  AND w.entity_type = l.entity_type AND position('/' in w.source_id) > 0
  AND split_part(w.source_id, '/', 2) = l.ts
  AND NOT EXISTS (SELECT 1 FROM import_workspace_id_map o
                  WHERE o.provider = w.provider AND o.source_workspace_name = w.source_workspace_name
                    AND o.entity_type = w.entity_type AND o.source_id = l.ts);

WITH lone AS (
    SELECT m.import_id, m.entity_type, split_part(m.source_id, '/', 2) AS ts
    FROM import_id_map m JOIN import_jobs j ON j.id = m.import_id AND j.provider = 'slack'
    WHERE m.entity_type IN ('message', 'comment') AND position('/' in m.source_id) > 0
    GROUP BY m.import_id, m.entity_type, split_part(m.source_id, '/', 2)
    HAVING count(*) = 1
)
UPDATE import_id_map m
SET source_id = l.ts,
    parent_source_id = CASE WHEN position('/' in COALESCE(m.parent_source_id, '')) > 0
                            THEN split_part(m.parent_source_id, '/', 2) ELSE m.parent_source_id END
FROM lone l
WHERE m.import_id = l.import_id AND m.entity_type = l.entity_type
  AND position('/' in m.source_id) > 0 AND split_part(m.source_id, '/', 2) = l.ts
  AND NOT EXISTS (SELECT 1 FROM import_id_map o
                  WHERE o.import_id = m.import_id AND o.entity_type = m.entity_type AND o.source_id = l.ts);
