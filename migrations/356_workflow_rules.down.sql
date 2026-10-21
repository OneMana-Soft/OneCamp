DROP TABLE IF EXISTS workflow_runs;
DROP INDEX IF EXISTS idx_workflows_project;
DELETE FROM workflows WHERE trigger_type IN ('task_assigned', 'task_field_changed');
ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_trigger_type_check;
ALTER TABLE workflows ADD CONSTRAINT workflows_trigger_type_check
    CHECK (trigger_type IN (
        'message_posted',
        'reaction_added',
        'user_joined_channel',
        'task_created',
        'task_status_changed',
        'meeting_ended'
    ));
ALTER TABLE workflows DROP COLUMN IF EXISTS rr_cursor;
ALTER TABLE workflows DROP COLUMN IF EXISTS paused_reason;
ALTER TABLE workflows DROP COLUMN IF EXISTS else_actions;
ALTER TABLE workflows DROP COLUMN IF EXISTS conditions;
ALTER TABLE workflows DROP COLUMN IF EXISTS project_id;
