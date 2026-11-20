-- A plain DROP, not CONCURRENTLY: the local sqlx behind `make migrate_down`
-- (0.8) runs a revert in a transaction whatever this file's marker says, and
-- dropping an index is quick.
DROP INDEX IF EXISTS idx_tasks_google_calendar_id;
