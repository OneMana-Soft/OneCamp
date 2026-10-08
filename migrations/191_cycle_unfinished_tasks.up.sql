-- Migration 191: the tasks a cycle hadn't finished when it was completed.
-- Completing a cycle moves its unfinished tasks to the next cycle (or out of
-- cycles), which left no trace of them in the cycle they came from: its
-- burndown would end at zero however much was left. Each such task keeps a row
-- here with when it had joined the cycle.
CREATE TABLE IF NOT EXISTS cycle_unfinished_tasks (
    "cycle_id"  uuid NOT NULL REFERENCES project_cycles(id) ON DELETE CASCADE,
    "task_uuid" uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    "added_at"  TIMESTAMP WITH TIME ZONE NOT NULL,
    PRIMARY KEY (cycle_id, task_uuid)
);
