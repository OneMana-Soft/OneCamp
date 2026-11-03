-- V-184 head 5a: the full unique gives way to 562's partial one, which holds
-- the same names among live views only. A full unique counts a view deleted
-- (kept, marked), so saving its name again would collide with it, and Save's
-- ON CONFLICT would write over the deleted view instead of making a new one.
-- Dropping builds nothing, so it runs in this migration's transaction.
ALTER TABLE task_views DROP CONSTRAINT IF EXISTS task_views_user_id_scope_name_key;
