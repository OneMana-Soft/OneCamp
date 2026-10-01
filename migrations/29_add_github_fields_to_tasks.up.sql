ALTER TABLE tasks ADD COLUMN IF NOT EXISTS "github_issue_number" int;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS "github_issue_url" text;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS "github_pr_number" int;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS "github_pr_url" text;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS "github_branch" varchar;

CREATE INDEX idx_tasks_github_issue ON tasks(github_issue_number) WHERE github_issue_number IS NOT NULL;
