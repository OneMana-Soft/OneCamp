-- Rollback migration 68.
DROP INDEX IF EXISTS idx_workspace_memory_deleted_reason;
ALTER TABLE workspace_memory_items DROP COLUMN IF EXISTS deleted_reason;
