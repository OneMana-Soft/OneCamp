-- no-transaction
-- Foreign key apps.bot_user_id -> users (no ON DELETE: a bot user can't be deleted while its app has it)
-- had no index leading with it, so deleting a row of users scanned all of apps.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_apps_bot_user_id ON apps (bot_user_id);
