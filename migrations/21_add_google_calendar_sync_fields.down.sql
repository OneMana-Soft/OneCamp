ALTER TABLE tasks DROP COLUMN IF EXISTS task_google_calendar_id;
ALTER TABLE integrations DROP COLUMN IF EXISTS task_sync_enabled;
