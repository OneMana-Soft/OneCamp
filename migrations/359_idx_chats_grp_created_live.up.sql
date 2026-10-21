-- no-transaction
-- A conversation's live messages by time: unread counts and boundaries
-- (domain/LastSeenChannel UnreadBoundaryInChat, the DM list) read grp_id with
-- created_at; idx_chats_grp_id_active has grp_id alone.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chats_grp_created_live ON chats (grp_id, created_at) WHERE deleted_at IS NULL;
