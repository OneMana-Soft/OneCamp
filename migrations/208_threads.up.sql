-- Anchored threads (docs programme A8): a conversation pinned to a place in a
-- resource. The thread lives here; its comments stay where each kind of
-- resource keeps them (a doc's in the graph, each tagged comment_thread_id).
-- Ids come from the app (business/Thread), so nothing here needs a uuid
-- extension, and all of it runs on Postgres 12.
--
-- The anchor is kept as it was made, and never moved by the server on its own
-- (D-9). A range of a live doc ('yrange') is two encoded Yjs relative
-- positions, the lineage they belong to and the quoted text; the web app
-- decides from those whether the thread still holds or is detached. Only
-- someone's explicit "Reattach here" writes a new anchor.
CREATE TABLE IF NOT EXISTS threads (
    id              uuid        PRIMARY KEY,
    resource_kind   text        NOT NULL CHECK (resource_kind ~ '^[a-z_]{1,32}$'),
    resource_uuid   text        NOT NULL CHECK (length(resource_uuid) BETWEEN 1 AND 128),
    anchor_kind     text        NOT NULL DEFAULT 'none'
                    CHECK (anchor_kind IN ('yrange', 'block', 'message', 'time', 'cell', 'none')),
    anchor_start    bytea,
    anchor_end      bytea,
    anchor_block_id text,
    anchor_lineage  text,
    anchor_quote    text,
    anchor_meta     jsonb,
    status          text        NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    resolved_by     uuid,
    resolved_at     timestamptz,
    task_uuid       uuid,
    created_by      uuid        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT NOW(),
    updated_at      timestamptz NOT NULL DEFAULT NOW(),
    -- A range has both ends; a block anchor names its block; the kinds that
    -- point into something else say where.
    CONSTRAINT threads_yrange_ends CHECK (anchor_kind <> 'yrange' OR (anchor_start IS NOT NULL AND anchor_end IS NOT NULL)),
    CONSTRAINT threads_block_named CHECK (anchor_kind <> 'block' OR anchor_block_id IS NOT NULL),
    CONSTRAINT threads_meta_given CHECK (anchor_kind NOT IN ('message', 'time', 'cell') OR anchor_meta IS NOT NULL),
    -- Resolved means someone resolved it, at a time; open means neither.
    CONSTRAINT threads_resolved_state CHECK ((status = 'resolved') = (resolved_at IS NOT NULL))
);
-- A resource's threads, open or resolved: the panel's one read.
CREATE INDEX IF NOT EXISTS idx_threads_resource ON threads (resource_kind, resource_uuid, status);

-- Who takes part in a thread, and how far each has read. The roles rank
-- author > replier > mentioned > reader, and a person keeps the highest they
-- have had: a reader only opened it, and is never told of its replies for that.
CREATE TABLE IF NOT EXISTS thread_participants (
    thread_id    uuid        NOT NULL REFERENCES threads (id) ON DELETE CASCADE,
    user_uuid    uuid        NOT NULL,
    role         text        NOT NULL CHECK (role IN ('author', 'replier', 'mentioned', 'reader')),
    last_read_at timestamptz,
    PRIMARY KEY (thread_id, user_uuid)
);
