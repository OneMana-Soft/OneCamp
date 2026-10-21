-- no-transaction
-- Foreign key project_status_order.set_by -> users (ON DELETE SET NULL) had no
-- index, so deleting a users row scanned all of project_status_order.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fk_project_status_order_set_by ON project_status_order (set_by);
