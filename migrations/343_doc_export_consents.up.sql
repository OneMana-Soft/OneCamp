-- Migration 343: owners letting their private docs into workspace exports
-- (docs programme C4, PROGRAM.md §8).
--
-- An admin's export of the workspace leaves out every private doc the admin
-- can't open. A doc's owner may let it in: one row per doc they let in, with
-- who let it in and when. The consent is the owner's: when the doc is handed
-- to someone else the row no longer counts (owner_uuid must still be the
-- doc's owner when an export reads it), and taking it back deletes the row.
-- Both are written to the admin audit log as well.
CREATE TABLE IF NOT EXISTS doc_export_consents (
    "doc_uuid"   uuid PRIMARY KEY,
    "owner_uuid" uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
