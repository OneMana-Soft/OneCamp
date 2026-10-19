-- Comments for threads on kinds that keep no comments of their own (tables
-- programme A8, with docs' anchored threads, migration 208): a table's rows
-- today, a call's timeline or a calendar event next. A doc's comments stay
-- where they are, in the graph; a kind opts in to this store through its
-- thread resource (business/Thread storedComments.go). Ids come from the app,
-- so nothing here needs a uuid extension, and all of it runs on Postgres 12.
--
-- A comment's body is the comment editor's HTML, read once through the content
-- codec before it is written (only what the editor's schema renders survives),
-- so what is stored is already safe to show. Removing a comment marks it: it
-- goes from every reader, and nothing is purged (owner questions O1 and O3).
CREATE TABLE IF NOT EXISTS thread_comments (
    id            uuid        PRIMARY KEY,
    -- The thread it is in. No foreign key, as threads have none to what they
    -- are on: the threads migration (208) stays free to go down and up on its
    -- own, and a thread is never deleted once a comment is written in it.
    thread_id     uuid        NOT NULL,
    -- The thread's resource, kept beside each comment so a resource's comments
    -- are one read (a row's record shows every thread on it).
    resource_kind text        NOT NULL CHECK (resource_kind ~ '^[a-z_]{1,32}$'),
    resource_uuid text        NOT NULL CHECK (length(resource_uuid) BETWEEN 1 AND 128),
    author_uuid   uuid        NOT NULL,
    -- Who wrote it: a person, or an agent acting for author_uuid (agent_uuid).
    author_kind   text        NOT NULL DEFAULT 'person' CHECK (author_kind IN ('person', 'agent')),
    agent_uuid    uuid,
    body          text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT NOW(),
    edited_at     timestamptz,
    deleted_at    timestamptz,
    deleted_by    uuid,
    CONSTRAINT thread_comments_agent_named CHECK ((author_kind = 'agent') = (agent_uuid IS NOT NULL)),
    CONSTRAINT thread_comments_deleted_state CHECK ((deleted_at IS NULL) = (deleted_by IS NULL))
);
-- A thread's comments in order, and a resource's live comments: the two reads.
CREATE INDEX IF NOT EXISTS idx_thread_comments_thread ON thread_comments (thread_id, created_at);
CREATE INDEX IF NOT EXISTS idx_thread_comments_resource ON thread_comments (resource_kind, resource_uuid, created_at)
    WHERE deleted_at IS NULL;

-- A reaction on a stored comment: one a person, which changing its emoji
-- replaces, as a doc comment's is.
CREATE TABLE IF NOT EXISTS thread_comment_reactions (
    id         uuid        PRIMARY KEY,
    comment_id uuid        NOT NULL REFERENCES thread_comments (id) ON DELETE CASCADE,
    user_uuid  uuid        NOT NULL,
    emoji_id   text        NOT NULL CHECK (emoji_id ~ '^[a-z0-9_+-]{1,64}$'),
    created_at timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT thread_comment_reactions_one_each UNIQUE (comment_id, user_uuid)
);
