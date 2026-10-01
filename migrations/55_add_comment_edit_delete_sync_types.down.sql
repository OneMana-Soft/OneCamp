-- Migration 55 down: Revert sync_type CHECK to previous values (without comment_edit, comment_delete)

ALTER TABLE github_sync_queue DROP CONSTRAINT IF EXISTS github_sync_queue_sync_type_check;
ALTER TABLE github_sync_queue ADD CONSTRAINT github_sync_queue_sync_type_check
    CHECK (sync_type IN ('status', 'name', 'description', 'assignee', 'label', 'comment'));
