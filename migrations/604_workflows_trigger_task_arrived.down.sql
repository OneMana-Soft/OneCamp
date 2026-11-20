-- The list 495 set, NOT VALID: task_arrived rules saved before the downgrade
-- may exist; they stay (no server without the trigger runs them) rather than
-- failing the step. Under the old list such a row can't be updated or
-- soft-deleted (Postgres checks every row a write changes) until an upgrade
-- adds task_arrived back.
ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_trigger_type_check;
ALTER TABLE workflows ADD CONSTRAINT workflows_trigger_type_check
    CHECK (trigger_type IN ('message_posted', 'reaction_added', 'user_joined_channel', 'task_created', 'task_status_changed', 'meeting_ended', 'task_assigned', 'task_field_changed', 'schedule', 'task_due_soon', 'task_overdue', 'form_submitted', 'approval_decided')) NOT VALID;
