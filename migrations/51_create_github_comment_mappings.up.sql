CREATE TABLE IF NOT EXISTS github_comment_mappings (
    id uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    github_comment_id bigint NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    comment_uuid uuid NOT NULL,
    task_uuid uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(github_comment_id, repo_owner, repo_name)
);

CREATE INDEX IF NOT EXISTS idx_github_comment_mappings_task ON github_comment_mappings(task_uuid);
CREATE INDEX IF NOT EXISTS idx_github_comment_mappings_comment ON github_comment_mappings(comment_uuid);
