-- Reverting drops any workflow already using the new kind, so remove those
-- rows first rather than leaving the constraint un-addable.
DELETE FROM workflows WHERE trigger_type = 'meeting_ended';

ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_trigger_type_check;
ALTER TABLE workflows ADD CONSTRAINT workflows_trigger_type_check
    CHECK (trigger_type IN (
        'message_posted',
        'reaction_added',
        'user_joined_channel',
        'task_created',
        'task_status_changed'
    ));
