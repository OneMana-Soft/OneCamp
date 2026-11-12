-- V-698: a money field's values are its project's admins' only. The Work
-- Change Log is read alike by every member (GET /work/changes, the live
-- messages, webhooks), so a money field's change now names the field and no
-- value: {"field_id", "refetch": true} (business/TaskField ForEveryone). The
-- changes it already keeps (30 days) lose their value the same way.
UPDATE task_changes c
SET diff = (c.diff - 'value') || '{"refetch": true}'::jsonb
FROM task_fields f
WHERE c.diff ? 'field_id'
  AND c.diff ? 'value'
  AND f.type = 'money'
  AND f.id::text = c.diff ->> 'field_id';
