-- Rollback migration 98.
ALTER TABLE ai_settings DROP COLUMN IF EXISTS local_only_mode;
