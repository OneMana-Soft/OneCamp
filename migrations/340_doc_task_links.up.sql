-- Migration 340: a to-do in a doc that became a task (docs programme C1).
--
-- Pressing ⌘⇧Enter on a to-do makes a task in a project and ties the two: the
-- to-do's line keeps the task's id (data-task-id), and this row says that the
-- doc holds a to-do for the task. When the task is done or opened again
-- anywhere (its board, its panel, the API), every doc that holds it ticks or
-- clears its box through the live doc (business/Task/taskDocSync.go), and a
-- tick in the doc changes the task.
--
-- A row is made only by the conversion, which checks that the person may edit
-- the doc and make tasks in the project. A save never adds one: a line pasted
-- from elsewhere with someone else's task id would otherwise tell this doc's
-- readers when a task they can't see is done. A row goes when a change to its
-- task finds the doc no longer holds the to-do.
CREATE TABLE IF NOT EXISTS doc_task_links (
    "doc_uuid"   uuid NOT NULL,
    "task_uuid"  uuid NOT NULL,
    "created_by" uuid NOT NULL,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    PRIMARY KEY (doc_uuid, task_uuid)
);

CREATE INDEX IF NOT EXISTS idx_doc_task_links_task ON doc_task_links(task_uuid);
