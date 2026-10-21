-- no-transaction
-- Who hears about a comment on a project (domain/UserProjectNotification): one
-- query per comment by project_id and notification_type; the unique index
-- leads with user_id. Also serves the project_id foreign key when a project is
-- deleted.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_users_project_notification_project_type ON users_project_notification (project_id, notification_type);
