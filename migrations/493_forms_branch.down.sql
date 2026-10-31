DELETE FROM workflows WHERE trigger_type = 'form_submitted';
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
        'task_overdue'
    ));
ALTER TABLE project_forms DROP COLUMN IF EXISTS confirmation;
