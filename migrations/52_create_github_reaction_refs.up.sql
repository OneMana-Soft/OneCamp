-- Migration 52: github_reaction_refs
-- Background: Migration 38 created this table with a different schema (task_id, user_id, github_name)
-- that is incompatible with the new schema needed for bidirectional reaction sync.
-- Production verification confirmed this table is empty, so we safely recreate it.

DO $$
DECLARE
    row_count integer;
BEGIN
    -- Only drop if the OLD schema exists (has task_id, not task_uuid)
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'github_reaction_refs' AND column_name = 'task_id'
    ) AND NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'github_reaction_refs' AND column_name = 'task_uuid'
    ) THEN
        -- Defense-in-depth: verify table is empty before dropping
        EXECUTE 'SELECT COUNT(*) FROM github_reaction_refs' INTO row_count;
        
        IF row_count = 0 THEN
            DROP TABLE github_reaction_refs CASCADE;
        ELSE
            -- Should never happen per production audit, but preserve data just in case
            ALTER TABLE github_reaction_refs RENAME TO github_reaction_refs_legacy_38;
        END IF;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS github_reaction_refs (
    id uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    github_reaction_id bigint NOT NULL,
    github_comment_id bigint NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    reaction_uuid uuid NOT NULL,
    comment_uuid uuid NOT NULL,
    task_uuid uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(github_reaction_id, github_comment_id, repo_owner, repo_name)
);

CREATE INDEX IF NOT EXISTS idx_github_reaction_refs_task ON github_reaction_refs(task_uuid);
CREATE INDEX IF NOT EXISTS idx_github_reaction_refs_comment ON github_reaction_refs(comment_uuid);
CREATE INDEX IF NOT EXISTS idx_github_reaction_refs_reaction ON github_reaction_refs(reaction_uuid);
