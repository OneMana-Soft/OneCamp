-- Doc versions that can be named, pinned and attributed (docs programme A4,
-- the Snapshot Service; reserved as 206, numbered in merge order).
--
-- doc_snapshots (migration 88) kept at most 15 versions a doc, about two and a
-- half hours of a busy one, credited only by user uuid. Versions are now kept by
-- tiers (every one for a day, hourly for a week, daily to the retention period)
-- within a byte cap per doc, and:
--   name, pinned       a version someone named or pinned is kept past pruning;
--   contributor_meta   who edited between this version and the one before:
--                      [{user_uuid} | {agent_uuid, asker_uuid}], so an agent's
--                      edit is credited to the agent and the person who asked;
--   kind               'doc', or 'agent_run' for the body before an agent's run,
--                      which its Revert restores;
--   run_id             that run;
--   lineage            the copy of the doc (the collaboration service's epoch)
--                      the version was taken from;
--   stored_bytes       the gzipped size in object storage, for the byte cap.
-- Reasons gain 'idle' (2 minutes after the last edit), 'saved' (kept by someone
-- on purpose) and 'before_write' (before a server-side write). Ids are made by
-- the API (Postgres 12 has no gen_random_uuid).

ALTER TABLE doc_snapshots
    ADD COLUMN IF NOT EXISTS name             varchar NULL,
    ADD COLUMN IF NOT EXISTS pinned           boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS contributor_meta jsonb   NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS kind             varchar NOT NULL DEFAULT 'doc',
    ADD COLUMN IF NOT EXISTS run_id           varchar NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS lineage          varchar NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS stored_bytes     integer NOT NULL DEFAULT 0;

ALTER TABLE doc_snapshots DROP CONSTRAINT IF EXISTS doc_snapshots_reason_check;
ALTER TABLE doc_snapshots ADD CONSTRAINT doc_snapshots_reason_check
    CHECK (reason IN ('interval', 'idle', 'mass_delete', 'manual', 'saved', 'before_write'));

ALTER TABLE doc_snapshots DROP CONSTRAINT IF EXISTS doc_snapshots_kind_check;
ALTER TABLE doc_snapshots ADD CONSTRAINT doc_snapshots_kind_check
    CHECK (kind IN ('doc', 'agent_run'));

ALTER TABLE doc_snapshots DROP CONSTRAINT IF EXISTS doc_snapshots_name_check;
ALTER TABLE doc_snapshots ADD CONSTRAINT doc_snapshots_name_check
    CHECK (name IS NULL OR char_length(name) BETWEEN 1 AND 120);

-- An agent run's versions, for Revert.
CREATE INDEX IF NOT EXISTS idx_doc_snapshots_run
    ON doc_snapshots (doc_uuid, run_id) WHERE run_id <> '';
