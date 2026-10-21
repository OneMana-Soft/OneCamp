-- Undoes 324: types first (tasks point at them), then numbers and keys.
DROP INDEX IF EXISTS idx_fk_tasks_task_type_id;
DROP INDEX IF EXISTS tasks_type;
ALTER TABLE tasks DROP COLUMN IF EXISTS task_type_id;
DROP INDEX IF EXISTS task_types_project;
DROP INDEX IF EXISTS task_types_project_name;
DROP TABLE IF EXISTS task_types;

DROP INDEX IF EXISTS tasks_unnumbered;
DROP INDEX IF EXISTS tasks_project_number;
ALTER TABLE tasks DROP COLUMN IF EXISTS task_number;

DROP INDEX IF EXISTS project_key_aliases_project;
DROP TABLE IF EXISTS project_key_aliases;
DROP INDEX IF EXISTS project_keys_key;
DROP TABLE IF EXISTS project_keys;
