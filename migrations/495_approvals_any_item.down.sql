-- Migration 495 down: approvals go back to task moves only. Requests about a
-- doc or a file, a link's decisions and the rules that start on an answer
-- go with it.
DELETE FROM workflows WHERE trigger_type = 'approval_decided';
ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_trigger_type_check;
ALTER TABLE workflows ADD CONSTRAINT workflows_trigger_type_check
    CHECK (trigger_type IN (
        'message_posted',
        'reaction_added',
        'user_joined_channel',
        'task_created',
        'task_status_changed',
        'meeting_ended',
        'task_assigned',
        'task_field_changed',
        'schedule',
        'task_due_soon',
        'task_overdue',
        'form_submitted'
    ));
ALTER TABLE client_reviews DROP CONSTRAINT IF EXISTS client_reviews_resource_uuid;
ALTER TABLE client_reviews DROP COLUMN IF EXISTS resource_uuid;
ALTER TABLE status_rules DROP COLUMN IF EXISTS client;
DELETE FROM approval_decisions WHERE grant_id IS NOT NULL;
ALTER TABLE approval_decisions DROP CONSTRAINT IF EXISTS approval_decisions_by;
DROP INDEX IF EXISTS idx_approval_decisions_grant_id;
DROP INDEX IF EXISTS uq_approval_decisions_grant;
DROP INDEX IF EXISTS uq_approval_decisions_user;
ALTER TABLE approval_decisions ALTER COLUMN user_id SET NOT NULL;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'approval_decisions_pkey') THEN
        ALTER TABLE approval_decisions ADD CONSTRAINT approval_decisions_pkey PRIMARY KEY (approval_id, user_id);
    END IF;
END $$;
ALTER TABLE approval_decisions DROP COLUMN IF EXISTS version;
ALTER TABLE approval_decisions DROP COLUMN IF EXISTS body_hash;
ALTER TABLE approval_decisions DROP COLUMN IF EXISTS name;
ALTER TABLE approval_decisions DROP COLUMN IF EXISTS grant_id;
DELETE FROM approvals WHERE resource_kind <> 'task' OR to_status IS NULL;
DROP INDEX IF EXISTS idx_approvals_due;
DROP INDEX IF EXISTS idx_approvals_resource;
DROP INDEX IF EXISTS uq_approvals_pending_resource;
CREATE UNIQUE INDEX IF NOT EXISTS uq_approvals_pending_task ON approvals (task_id) WHERE state = 'pending';
ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_resource;
ALTER TABLE approvals DROP COLUMN IF EXISTS client_grants;
ALTER TABLE approvals DROP COLUMN IF EXISTS reminded_at;
ALTER TABLE approvals DROP COLUMN IF EXISTS due_at;
ALTER TABLE approvals DROP COLUMN IF EXISTS ask;
ALTER TABLE approvals ALTER COLUMN to_status SET NOT NULL;
ALTER TABLE approvals ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE approvals ALTER COLUMN task_id SET NOT NULL;
ALTER TABLE approvals DROP COLUMN IF EXISTS resource_id;
ALTER TABLE approvals DROP COLUMN IF EXISTS resource_kind;
