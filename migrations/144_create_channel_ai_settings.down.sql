-- Rollback migration 144.
DROP INDEX IF EXISTS idx_channel_ai_settings_team_report;
DROP TABLE IF EXISTS channel_ai_settings;
