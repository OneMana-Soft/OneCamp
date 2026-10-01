ALTER TABLE tasks ADD COLUMN IF NOT EXISTS github_last_synced_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_tasks_github_issue_url ON tasks(github_issue_url) WHERE github_issue_url IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_github_pr_url ON tasks(github_pr_url) WHERE github_pr_url IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_github_branch ON tasks(github_branch) WHERE github_branch IS NOT NULL AND project_id IS NOT NULL;
