CREATE TABLE IF NOT EXISTS github_links (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "project_id" uuid NOT NULL REFERENCES projects(id),
    "repo_owner" varchar NOT NULL,
    "repo_name" varchar NOT NULL,
    "installation_id" bigint,
    "webhook_secret" varchar,
    "sync_issues" boolean DEFAULT true,
    "sync_prs" boolean DEFAULT true,
    "auto_create_tasks" boolean DEFAULT false,
    "default_task_status" varchar DEFAULT 'backlog',
    "label_mapping" jsonb,
    "created_by" uuid NOT NULL REFERENCES users(id),
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);

CREATE UNIQUE INDEX idx_github_links_project_repo ON github_links(project_id, repo_owner, repo_name) WHERE deleted_at IS NULL;
CREATE INDEX idx_github_links_project_id ON github_links(project_id) WHERE deleted_at IS NULL;
