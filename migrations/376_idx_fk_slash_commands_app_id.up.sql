-- no-transaction
-- Foreign key slash_commands.app_id -> apps (ON DELETE CASCADE) had no index,
-- so deleting a apps row scanned all of slash_commands.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_slash_commands_app_id ON slash_commands (app_id);
