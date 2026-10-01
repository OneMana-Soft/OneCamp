DROP INDEX IF EXISTS idx_tasks_github_issue;
ALTER TABLE tasks DROP COLUMN IF EXISTS "github_branch";
ALTER TABLE tasks DROP COLUMN IF EXISTS "github_pr_url";
ALTER TABLE tasks DROP COLUMN IF EXISTS "github_pr_number";
ALTER TABLE tasks DROP COLUMN IF EXISTS "github_issue_url";
ALTER TABLE tasks DROP COLUMN IF EXISTS "github_issue_number";
