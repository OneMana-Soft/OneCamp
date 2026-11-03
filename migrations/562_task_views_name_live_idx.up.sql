-- no-transaction
-- V-184 head 5a: a person's saved view names are unique in a scope among live
-- views only, so a view saved under a deleted view's name (kept, marked) is a
-- new row. It replaces task_views_user_id_scope_name_key (every row), which
-- 564 drops once this one and 563's index on user_id are built.
-- CONCURRENTLY: built without blocking writes; one statement per file,
-- outside a transaction (sqlx reads the first line). A build that fails
-- leaves an INVALID index: drop it and run the migration again.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_task_views_name_live ON task_views (user_id, scope, name) WHERE deleted_at IS NULL;
