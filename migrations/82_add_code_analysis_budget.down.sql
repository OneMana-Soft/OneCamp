-- Rollback migration 82.
ALTER TABLE ai_settings DROP COLUMN IF EXISTS "code_analysis_max_files";
