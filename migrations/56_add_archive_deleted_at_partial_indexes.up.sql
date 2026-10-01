-- Add partial indexes to avoid sequential scans on archive / undo / stats queries.
-- Up: create indexes. Down: drop indexes.

CREATE INDEX IF NOT EXISTS idx_posts_deleted_at_archived ON posts(deleted_at DESC) WHERE deleted_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_posts_deleted_at_active_created_at ON posts(created_at) WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_chats_deleted_at_archived ON chats(deleted_at DESC) WHERE deleted_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_chats_deleted_at_active_created_at ON chats(created_at) WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_tasks_deleted_at_archived ON tasks(deleted_at DESC) WHERE deleted_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_deleted_at_active_created_at ON tasks(created_at) WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_attachments_deleted_at_archived ON attachments(deleted_at DESC) WHERE deleted_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_attachments_deleted_at_active_created_at ON attachments(created_at) WHERE deleted_at IS NULL;
