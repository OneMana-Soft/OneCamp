-- Drop partial indexes added in migration 56.

DROP INDEX IF EXISTS idx_posts_deleted_at_archived;
DROP INDEX IF EXISTS idx_posts_deleted_at_active_created_at;

DROP INDEX IF EXISTS idx_chats_deleted_at_archived;
DROP INDEX IF EXISTS idx_chats_deleted_at_active_created_at;

DROP INDEX IF EXISTS idx_tasks_deleted_at_archived;
DROP INDEX IF EXISTS idx_tasks_deleted_at_active_created_at;

DROP INDEX IF EXISTS idx_attachments_deleted_at_archived;
DROP INDEX IF EXISTS idx_attachments_deleted_at_active_created_at;
