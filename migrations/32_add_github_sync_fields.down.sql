DROP INDEX IF EXISTS idx_tasks_github_branch;
DROP INDEX IF EXISTS idx_tasks_github_pr_url;
DROP INDEX IF EXISTS idx_tasks_github_issue_url;
ALTER TABLE tasks DROP COLUMN IF EXISTS github_last_synced_at;
