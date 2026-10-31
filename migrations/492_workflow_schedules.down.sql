DROP INDEX IF EXISTS scheduled_jobs_workflow_schedule_idx;
UPDATE scheduled_jobs SET status = 'cancelled', updated_at = NOW()
    WHERE job_type = 'workflow_schedule' AND status IN ('pending', 'running') AND deleted_at IS NULL;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS matched;
DELETE FROM workflows WHERE trigger_type IN ('schedule', 'task_due_soon', 'task_overdue');
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
        'task_field_changed'
    ));
