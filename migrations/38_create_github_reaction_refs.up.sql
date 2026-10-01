CREATE TABLE IF NOT EXISTS github_reaction_refs (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "task_id" uuid NOT NULL REFERENCES tasks(id),
    "user_id" uuid NOT NULL REFERENCES users(id),
    "github_name" text NOT NULL,
    "github_reaction_id" bigint NOT NULL,
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(task_id, user_id, github_name)
);

CREATE INDEX idx_github_reaction_refs_task ON github_reaction_refs(task_id);
CREATE INDEX idx_github_reaction_refs_lookup ON github_reaction_refs(task_id, user_id, github_name);
