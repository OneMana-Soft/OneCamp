-- Track GitHub sync health per task
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS github_sync_status varchar(20) DEFAULT 'synced' CHECK (github_sync_status IN ('pending', 'synced', 'failed'));
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS github_sync_error text;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS github_sync_attempts int DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_tasks_github_sync_status ON tasks(github_sync_status) WHERE github_sync_status IS NOT NULL;
