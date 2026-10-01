CREATE TABLE IF NOT EXISTS github_task_activities (
    id uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    task_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    activity_type varchar NOT NULL CHECK (activity_type IN ('comment','reaction','pr_opened','pr_closed','pr_merged','issue_opened','issue_closed','issue_reopened','branch_created','commit_pushed','status_synced','assignee_synced','label_synced')),
    github_login text,
    github_avatar_url text,
    github_html_url text,
    title text,
    body text,
    payload jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_github_task_activities_task_id ON github_task_activities(task_id);
CREATE INDEX IF NOT EXISTS idx_github_task_activities_created_at ON github_task_activities(created_at);
