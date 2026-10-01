-- Rollback migration 80.
ALTER TABLE ai_settings DROP COLUMN IF EXISTS "coworker_enabled";
