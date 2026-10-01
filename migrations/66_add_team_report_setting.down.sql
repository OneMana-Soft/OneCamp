-- Rollback migration 66.
ALTER TABLE ai_settings DROP COLUMN IF EXISTS "team_report_enabled";
