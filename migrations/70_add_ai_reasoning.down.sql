-- Rollback migration 70.
ALTER TABLE ai_settings DROP COLUMN IF EXISTS reasoning_enabled;
