-- Migration 118: record WHO triggered a durable agent job.
--
-- ai_agent_tasks.run_as_user_id is the agent's OWNER (whose permissions the run
-- executes under). triggered_by is the person who actually asked — the message
-- author for a channel/DM/group @mention. It lets the durable worker notify the
-- RIGHT human (an in-app activity ping) when a background run blocks awaiting
-- input or finishes, so a teammate isn't left refreshing a thread. Nullable:
-- schedule/event/task-assignment jobs have no single human trigger, and older
-- rows simply carry NULL (no notification target), so nothing regresses.
ALTER TABLE ai_agent_tasks
    ADD COLUMN IF NOT EXISTS triggered_by uuid;
