ALTER TABLE tasks ADD COLUMN IF NOT EXISTS "task_assignee" uuid REFERENCES users(id);
