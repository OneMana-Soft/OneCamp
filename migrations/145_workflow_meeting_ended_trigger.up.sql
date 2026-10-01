-- Let a workflow fire when a call finishes.
--
-- The recap agent has turned finished calls into a summary for a while, but
-- nothing else could react to one. A meeting is the moment most likely to
-- produce work — a decision to record, a task to file, a status update somebody
-- is waiting on — and it was the one workspace event with no trigger.
--
-- Kept a CHECK rather than an enum, as migration 77 established, so adding a
-- kind stays a one-line change.
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
