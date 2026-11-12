-- An event's files (calendar gcal-1a head B, business/Calendar eventFiles.go):
-- the OneCamp docs and boards an event carries, by reference, where Google
-- attaches Drive files. A file uploaded in the doc tree is a doc, so it is
-- carried as one. Nothing is uploaded here and no title is kept: each reader
-- names a file through their own chip read (POST /entities), as they may.
--
-- A repeating event's files are its series' (event_id is the series' row),
-- so every instance carries them.
--
-- Nothing here is ever deleted by the app. Taking a file off stamps
-- deleted_at and deleted_by; adding it again brings the same row back. An
-- event deleted for good (its own rows' purge) takes its rows with it.
--
-- ref_uuid has no foreign key: a doc and a board are Dgraph nodes, and an
-- event's file must never block one from being deleted or restored.

CREATE TABLE IF NOT EXISTS calendar_event_files (
    "id"         uuid PRIMARY KEY NOT NULL,
    "event_id"   uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    "ref_kind"   text NOT NULL,
    "ref_uuid"   uuid NOT NULL,
    "added_by"   uuid NOT NULL,
    "added_at"   timestamptz NOT NULL DEFAULT NOW(),
    "deleted_at" timestamptz,
    "deleted_by" uuid,
    CONSTRAINT calendar_event_files_kind_check CHECK (ref_kind IN ('doc', 'board'))
);

-- Every row of an event, removed ones included, so the foreign key above is
-- indexed whole (tests/integration/fk_indexes_test.go); every read of an
-- event's files starts here.
CREATE INDEX IF NOT EXISTS calendar_event_files_event_idx
    ON calendar_event_files (event_id);

-- One live row per event and thing: adding one already on it changes
-- nothing, and one taken off is brought back rather than added twice.
CREATE UNIQUE INDEX IF NOT EXISTS calendar_event_files_live_idx
    ON calendar_event_files (event_id, ref_kind, ref_uuid) WHERE deleted_at IS NULL;
