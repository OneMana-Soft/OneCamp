-- no-transaction
-- Foreign key ai_mcp_servers.created_by -> users (ON DELETE SET NULL) had no
-- index, so deleting a users row scanned all of ai_mcp_servers.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_ai_mcp_servers_created_by ON ai_mcp_servers (created_by);
