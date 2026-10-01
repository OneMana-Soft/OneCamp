-- Normalize deleted_at sentinel from '1970-01-01 00:00:00+00' to NULL for active rows.
-- This aligns all entity types with the standard SQL soft-delete convention (NULL = active).
-- The admin bulk archiver and RestoreItems will use IS NULL / IS NOT NULL going forward.

UPDATE posts SET deleted_at = NULL WHERE deleted_at = '1970-01-01 00:00:00+00';
UPDATE chats SET deleted_at = NULL WHERE deleted_at = '1970-01-01 00:00:00+00';
UPDATE tasks SET deleted_at = NULL WHERE deleted_at = '1970-01-01 00:00:00+00';
UPDATE teams SET deleted_at = NULL WHERE deleted_at = '1970-01-01 00:00:00+00';
UPDATE projects SET deleted_at = NULL WHERE deleted_at = '1970-01-01 00:00:00+00';
UPDATE attachments SET deleted_at = NULL WHERE deleted_at = '1970-01-01 00:00:00+00';
