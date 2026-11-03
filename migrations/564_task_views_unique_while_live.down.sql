-- The full unique counts every row, so the views deleted go first: one saved
-- again under their name would collide.
DELETE FROM task_views WHERE deleted_at IS NOT NULL;
ALTER TABLE task_views ADD CONSTRAINT task_views_user_id_scope_name_key UNIQUE (user_id, scope, name);
