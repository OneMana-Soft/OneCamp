DROP INDEX IF EXISTS idx_tasks_github_pr_state;

ALTER TABLE tasks
DROP COLUMN IF EXISTS github_pr_state,
DROP COLUMN IF EXISTS github_pr_check_status,
DROP COLUMN IF EXISTS github_pr_review_state,
DROP COLUMN IF EXISTS github_pr_is_draft;

ALTER TABLE github_links
DROP COLUMN IF EXISTS automation_rules,
DROP COLUMN IF EXISTS branch_format;
