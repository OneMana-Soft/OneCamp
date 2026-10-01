-- Revert deleted_at sentinel from NULL back to '1970-01-01 00:00:00+00' for active rows.
-- Only affects rows where deleted_at IS NULL (i.e., currently active).

UPDATE posts SET deleted_at = '1970-01-01 00:00:00+00' WHERE deleted_at IS NULL;
UPDATE chats SET deleted_at = '1970-01-01 00:00:00+00' WHERE deleted_at IS NULL;
UPDATE tasks SET deleted_at = '1970-01-01 00:00:00+00' WHERE deleted_at IS NULL;
UPDATE teams SET deleted_at = '1970-01-01 00:00:00+00' WHERE deleted_at IS NULL;
UPDATE projects SET deleted_at = '1970-01-01 00:00:00+00' WHERE deleted_at IS NULL;
UPDATE attachments SET deleted_at = '1970-01-01 00:00:00+00' WHERE deleted_at IS NULL;
