-- Rollback migration 69.
ALTER TABLE ai_settings DROP COLUMN IF EXISTS context_window_tokens;
