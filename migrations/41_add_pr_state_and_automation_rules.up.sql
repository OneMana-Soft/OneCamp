-- Add PR state tracking and automation rules to github_links
ALTER TABLE github_links
ADD COLUMN IF NOT EXISTS automation_rules jsonb NOT NULL DEFAULT '{
  "pr_drafted": "todo",
  "pr_opened": "inProgress",
  "review_requested": "inReview",
  "changes_requested": "inProgress",
  "approved": "inReview",
  "pr_merged": "done",
  "pr_closed_without_merge": "canceled"
}'::jsonb,
ADD COLUMN IF NOT EXISTS branch_format varchar DEFAULT 'feature/{taskId}-{slug}';

-- Add PR state tracking to tasks
ALTER TABLE tasks
ADD COLUMN IF NOT EXISTS github_pr_state varchar,
ADD COLUMN IF NOT EXISTS github_pr_check_status varchar,
ADD COLUMN IF NOT EXISTS github_pr_review_state varchar,
ADD COLUMN IF NOT EXISTS github_pr_is_draft boolean DEFAULT false;

CREATE INDEX IF NOT EXISTS idx_tasks_github_pr_state ON tasks(github_pr_state) WHERE github_pr_state IS NOT NULL;
