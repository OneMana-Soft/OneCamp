-- Project names go back to being unique across the workspace only while no
-- two teams share one. Otherwise this refuses before anything is undone, and
-- says which names repeat: renaming someone's project to make room is theirs
-- to decide, not a migration's.
DO $$
DECLARE
    repeated text;
    how_many int;
BEGIN
    SELECT COUNT(*) INTO how_many
      FROM (SELECT project_name FROM projects GROUP BY project_name HAVING COUNT(*) > 1) names;
    IF how_many > 0 THEN
        SELECT string_agg(quote_literal(project_name), ', ' ORDER BY project_name) INTO repeated
          FROM (SELECT project_name FROM projects GROUP BY project_name HAVING COUNT(*) > 1
                ORDER BY project_name LIMIT 20) names;
        IF how_many > 20 THEN
            repeated := repeated || format(' and %s more', how_many - 20);
        END IF;
        RAISE EXCEPTION 'projects: project names can''t be unique across the workspace again, because these names are used in more than one team: %. Rename all but one project of each name, then undo this migration again.', repeated;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'projects_project_name_key') THEN
        ALTER TABLE projects ADD CONSTRAINT projects_project_name_key UNIQUE (project_name);
    END IF;
END $$;

DELETE FROM system_configs WHERE key = 'projects.members_edit.since';
DROP TABLE IF EXISTS user_notices_seen;
DROP TABLE IF EXISTS project_roles;
