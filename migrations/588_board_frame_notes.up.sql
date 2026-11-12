-- Migration 588: presenter notes (miro-28 2a; business/Board notes.go). A
-- frame's notes on a board, which its presenter sees while presenting and the
-- audience never does. They live here, never in the board's document, because
-- a share-link guest receives the whole document and every export carries it.
-- Read by who may present the board (business/Board CanPresent), written with
-- its edit right; each write names the version it read, so a stale one is
-- refused. The body is capped and never indexed (the btree entry limit). One
-- live note per frame; deleting one is soft (deleted_at, deleted_by). A new
-- table, so its index builds here, on no rows. Ids are made in Go.
CREATE TABLE IF NOT EXISTS board_frame_notes (
    "id"         uuid PRIMARY KEY,
    "board_uuid" uuid NOT NULL,
    "frame_id"   varchar(128) NOT NULL CHECK (char_length(frame_id) BETWEEN 1 AND 128),
    "body"       text NOT NULL CHECK (char_length(body) BETWEEN 1 AND 10000),
    "version"    integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    "created_by" uuid NOT NULL,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_by" uuid NOT NULL,
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE,
    "deleted_by" uuid
);
-- A board's live notes, one per frame; reads are by board, writes by frame.
CREATE UNIQUE INDEX IF NOT EXISTS board_frame_notes_live ON board_frame_notes (board_uuid, frame_id)
    WHERE deleted_at IS NULL;
