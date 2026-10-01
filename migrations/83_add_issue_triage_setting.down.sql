-- Rollback migration 83.
ALTER TABLE ai_settings DROP COLUMN IF EXISTS "issue_triage_enabled";
