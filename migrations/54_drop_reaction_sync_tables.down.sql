-- Migration 54 down: Recreate reaction sync tables (for rollback only)
-- NOTE: This is a best-effort rollback. Data is lost on the up migration.

CREATE TABLE IF NOT EXISTS emoji_sync_map (
    "github_name" text PRIMARY KEY,
    "onecamp_uuid" text NOT NULL
);

CREATE TABLE IF NOT EXISTS github_reaction_refs (
    id uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    github_reaction_id bigint NOT NULL,
    github_comment_id bigint NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    reaction_uuid text,
    comment_uuid uuid NOT NULL,
    task_uuid uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(github_reaction_id, github_comment_id, repo_owner, repo_name)
);

CREATE INDEX IF NOT EXISTS idx_github_reaction_refs_task ON github_reaction_refs(task_uuid);
CREATE INDEX IF NOT EXISTS idx_github_reaction_refs_comment ON github_reaction_refs(comment_uuid);
CREATE INDEX IF NOT EXISTS idx_github_reaction_refs_reaction ON github_reaction_refs(reaction_uuid);
