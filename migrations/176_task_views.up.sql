-- Migration 176: saved task views. A person names a set of filters, sort and
-- columns on a project's task list (scope 'project:<uuid>') or on My Tasks
-- (scope 'mine') and comes back to it in one click. Private to the person;
-- saving a name again replaces that view.
CREATE TABLE IF NOT EXISTS task_views (
    "id"         uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "user_id"    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "scope"      varchar NOT NULL,
    "name"       varchar NOT NULL,
    "state"      jsonb NOT NULL,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, scope, name)
);
