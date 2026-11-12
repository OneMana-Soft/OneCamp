-- Undoes 582: the tasks that were epics become plain tasks (task_type_id is
-- ON DELETE SET NULL); their tasks stay under them as sub-tasks.
DELETE FROM task_types WHERE id = '0c06e5fa-fd1a-41d1-bb38-7cdb1fe8097e';
