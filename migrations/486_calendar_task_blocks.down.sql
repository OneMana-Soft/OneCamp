ALTER TABLE events DROP CONSTRAINT IF EXISTS events_task_only_on_blocks;
DROP INDEX IF EXISTS events_task_idx;
ALTER TABLE events DROP COLUMN IF EXISTS "task_uuid";
