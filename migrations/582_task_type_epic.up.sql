-- Epics (jira-01 J-H, the work hierarchy): the seventh built-in task type. An
-- epic is a type plus the one parent/sub-task relation the graph already
-- keeps (task_parent_task / task_sub_tasks): its tasks hang off it there, and
-- their sub-tasks off them (business/WorkTaxonomy, domain/Task hierarchy.go).
-- Its id never changes: business/WorkTaxonomy names it TypeEpic.
INSERT INTO task_types (id, builtin, name, icon, position) VALUES
    ('0c06e5fa-fd1a-41d1-bb38-7cdb1fe8097e', 'epic', 'Epic', 'epic', 6)
ON CONFLICT (id) DO NOTHING;
