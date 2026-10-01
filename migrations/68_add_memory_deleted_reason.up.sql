-- Migration 68: distinguish WHY a workspace-memory item was soft-deleted.
--
-- The memory layer cascades on source-content removal so deleted content
-- can't linger as AI-queryable facts. But "removal" has two flavors with
-- opposite reversibility:
--
--   * user delete  — a user deletes their own message. Permanent; the
--                     derived memory should stay gone.
--   * archive       — an admin/retention sweep soft-deletes old content.
--                     REVERSIBLE: restore/undo brings the content back, so
--                     the derived memory must come back too.
--
-- To restore precisely (revive ONLY items the archive cascade removed, never
-- ones a user independently hard-deleted) we tag each soft-delete with its
-- reason. This mirrors the OpenSearch cascade's `deleted_by` marker, where
-- unarchive only revives docs its own cascade archived.
--
-- NULL reason == legacy/user delete (default), so existing rows and the
-- user-delete path need no change.

ALTER TABLE workspace_memory_items
    ADD COLUMN IF NOT EXISTS deleted_reason varchar;

-- Partial index for the archive restore path: "revive items deleted by the
-- archive cascade for this source". Tiny — only soft-deleted rows qualify.
CREATE INDEX IF NOT EXISTS idx_workspace_memory_deleted_reason
    ON workspace_memory_items (source_type, source_uuid, deleted_reason)
    WHERE deleted_at IS NOT NULL;
