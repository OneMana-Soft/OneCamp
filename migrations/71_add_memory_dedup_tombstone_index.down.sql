-- Rollback migration 71.
DROP INDEX IF EXISTS idx_workspace_memory_dedup_deleted;
DROP INDEX IF EXISTS idx_workspace_memory_project_all;
DROP INDEX IF EXISTS idx_workspace_memory_grp_all;
