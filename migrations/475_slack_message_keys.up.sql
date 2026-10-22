-- 475 is a placeholder: the integrator numbers this migration at merge.
--
-- A Slack message is known by its conversation and its ts (business/SlackImport
-- msgkey.go): Slack's ts is unique within a conversation, not across them, so a
-- message known by its ts alone was dropped when another conversation held one at
-- that ts, and a reply could be filed in another conversation's thread. The import
-- now keys its rows "<conversation id>/<ts>". The rows imports wrote before are
-- re-keyed here where their conversation can be told, so a re-import still finds
-- what came across; a row that can't be told (a placeholder, a reply whose thread
-- ts two conversations hold) keeps its bare ts and is found by nothing new.

-- A channel's messages and lines say their channel.
UPDATE import_id_map m
SET source_id = (m.metadata->>'channel_slack_id') || '/' || m.source_id
FROM import_jobs j
WHERE j.id = m.import_id AND j.provider = 'slack'
  AND m.entity_type = 'message' AND position('/' in m.source_id) = 0
  AND COALESCE(m.metadata->>'channel_slack_id', '') <> '';

-- A DM's or group DM's messages say its grouping, and its row in the same import
-- says which Slack conversation that is.
UPDATE import_id_map m
SET source_id = d.source_id || '/' || m.source_id
FROM import_id_map d, import_jobs j
WHERE j.id = m.import_id AND j.provider = 'slack'
  AND m.entity_type = 'message' AND position('/' in m.source_id) = 0
  AND COALESCE(m.metadata->>'grouping_id', '') <> ''
  AND d.import_id = m.import_id AND d.entity_type IN ('dm', 'mpim')
  AND d.metadata->>'grouping_id' = m.metadata->>'grouping_id';

-- A reply names its thread's ts; the one message in its import at that ts, now
-- keyed, says the conversation.
WITH parent AS (
    SELECT import_id, split_part(source_id, '/', 2) AS ts, min(source_id) AS key
    FROM import_id_map
    WHERE entity_type = 'message' AND position('/' in source_id) > 0
    GROUP BY import_id, split_part(source_id, '/', 2)
    HAVING count(*) = 1
)
UPDATE import_id_map c
SET source_id = split_part(p.key, '/', 1) || '/' || c.source_id,
    parent_source_id = p.key
FROM parent p, import_jobs j
WHERE j.id = c.import_id AND j.provider = 'slack'
  AND c.entity_type = 'comment' AND position('/' in c.source_id) = 0
  AND p.import_id = c.import_id AND p.ts = c.parent_source_id;

-- The workspace's map takes the key its row's import now gives the same message.
UPDATE import_workspace_id_map w
SET source_id = m.source_id
FROM import_id_map m
WHERE w.provider = 'slack' AND w.entity_type IN ('message', 'comment')
  AND position('/' in w.source_id) = 0
  AND m.import_id = w.origin_import_id AND m.entity_type = w.entity_type
  AND m.onecamp_uuid = w.onecamp_uuid AND position('/' in m.source_id) > 0;

-- Replies an import filed in another conversation's thread before this are found
-- and moved by business/SlackImport repair.go, once per import: replies_checked_at
-- is when it checked one. Imports from now on file every reply in its own
-- conversation, so a new one starts checked; the ones before start unchecked, and
-- the weekly clean-up keeps an unchecked import's export until the repair has read it.
ALTER TABLE import_jobs ADD COLUMN IF NOT EXISTS replies_checked_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE import_jobs ALTER COLUMN replies_checked_at SET DEFAULT NOW();
