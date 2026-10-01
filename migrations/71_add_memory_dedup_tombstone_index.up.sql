-- Migration 71: fast tombstone lookup for the memory dedup guard.
--
-- The workspace-memory upsert now honors USER deletions: if a user
-- permanently deleted a fact, automatic re-extraction of the same
-- conversation must NOT silently re-create it (a "tombstone"). To decide
-- this, Upsert looks up soft-deleted rows by dedup_hash before inserting.
--
-- The existing unique dedup index is partial on `deleted_at IS NULL` (live
-- rows only), so it can't serve the tombstone lookup, which inspects
-- soft-DELETED rows. This partial index covers exactly that hot path:
-- "is there a soft-deleted row with this hash, and why was it removed?"
-- — tiny, since only deleted rows qualify.
--
-- Idempotent; existing rows unaffected.

CREATE INDEX IF NOT EXISTS idx_workspace_memory_dedup_deleted
    ON workspace_memory_items (dedup_hash, deleted_at DESC)
    WHERE deleted_at IS NOT NULL;

-- Scope-level cascade hot paths: "archive / delete / restore every item in
-- this project or DM/group". The channel scope is already covered by
-- idx_workspace_memory_channel (migration 65); add the project and group
-- equivalents so a whole-scope cascade doesn't scan the table.
CREATE INDEX IF NOT EXISTS idx_workspace_memory_project_all
    ON workspace_memory_items (project_uuid)
    WHERE project_uuid IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_workspace_memory_grp_all
    ON workspace_memory_items (chat_grp_id)
    WHERE chat_grp_id IS NOT NULL;
