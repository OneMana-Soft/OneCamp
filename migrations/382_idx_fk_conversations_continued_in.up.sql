-- no-transaction
-- Foreign key conversations.continued_in -> conversations (ON DELETE SET NULL)
-- had no index, so deleting a conversations row scanned all of conversations.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_conversations_continued_in ON conversations (continued_in);
