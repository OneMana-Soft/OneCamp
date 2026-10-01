DROP INDEX IF EXISTS idx_tasks_github_sync_status;
ALTER TABLE tasks DROP COLUMN IF EXISTS github_sync_status;
ALTER TABLE tasks DROP COLUMN IF EXISTS github_sync_error;
ALTER TABLE tasks DROP COLUMN IF EXISTS github_sync_attempts;
