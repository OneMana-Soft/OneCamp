-- Rollback migration 126: drop external data sources.
DROP INDEX IF EXISTS idx_data_sources_created_by;
DROP INDEX IF EXISTS idx_data_sources_active;
DROP TABLE IF EXISTS data_sources;
