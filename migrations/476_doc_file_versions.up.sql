-- Migration 476: a doc tree file's versions (docs F1, Docs & Files). The
-- integrator gives it the next free number at landing: beta's highest was
-- 474, and v2.75.1's slack_message_keys takes 475.
--
-- A file is a doc node (doc_node_kind "file"). Each of its versions is its own
-- attachments row, at place doc with src_value the file node, so FileAccess's
-- doc rule answers every version and nothing new decides access. A row here
-- names that attachment with what Postgres keeps of no attachment: its name,
-- size and type, an optional note, who added it and when. The node's file is
-- its highest n. A purge of the node takes its rows and archives their
-- attachments (models/postgres/DocFile ForgetFiles), so the attachments purge
-- takes the bytes. created_by names the person as business/DataSubject's
-- inventory finds personal data. Nothing here needs a uuid made by Postgres,
-- so it all runs on Postgres 12.
CREATE TABLE IF NOT EXISTS doc_file_versions (
    doc_uuid      uuid        NOT NULL,
    n             integer     NOT NULL CHECK (n > 0),
    attachment_id uuid        NOT NULL,
    name          text        NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
    size_bytes    bigint      NOT NULL CHECK (size_bytes >= 0),
    mime          text        NOT NULL CHECK (length(mime) BETWEEN 1 AND 255),
    note          text        CHECK (note IS NULL OR length(note) <= 500),
    created_by    uuid        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (doc_uuid, n)
);

-- A version's attachment is that version's alone.
CREATE UNIQUE INDEX IF NOT EXISTS idx_doc_file_versions_attachment ON doc_file_versions (attachment_id);
