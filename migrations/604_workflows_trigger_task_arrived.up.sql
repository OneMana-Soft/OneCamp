-- linear-21 H2: a rule may start on work arriving in triage (task_arrived).
-- The list 495 set plus task_arrived, added as 495 did: checked as it's added, in one transaction (a small table).
ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_trigger_type_check;
ALTER TABLE workflows ADD CONSTRAINT workflows_trigger_type_check
    CHECK (trigger_type IN ('message_posted', 'reaction_added', 'user_joined_channel', 'task_created', 'task_status_changed', 'meeting_ended', 'task_assigned', 'task_field_changed', 'schedule', 'task_due_soon', 'task_overdue', 'form_submitted', 'approval_decided', 'task_arrived'));
