ALTER TABLE tasks ADD COLUMN IF NOT EXISTS task_google_calendar_id text;
ALTER TABLE integrations ADD COLUMN IF NOT EXISTS task_sync_enabled boolean DEFAULT false;
