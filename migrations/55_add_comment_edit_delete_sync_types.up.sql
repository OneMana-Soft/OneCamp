-- Migration 55: Add comment_edit and comment_delete to github_sync_queue sync_type CHECK
-- Background: Comment edit and delete outbound sync was added but the CHECK constraint
-- rejects sync_type values not in the enum list.

ALTER TABLE github_sync_queue DROP CONSTRAINT IF EXISTS github_sync_queue_sync_type_check;
ALTER TABLE github_sync_queue ADD CONSTRAINT github_sync_queue_sync_type_check
    CHECK (sync_type IN ('status', 'name', 'description', 'assignee', 'label', 'comment', 'comment_edit', 'comment_delete'));
