-- Rollback for migration 60 (squashed import pipeline).
-- Drops everything created by the up migration in reverse dependency order.

DROP TABLE IF EXISTS import_oauth_tokens;
DROP TABLE IF EXISTS import_priority_mappings;
DROP TABLE IF EXISTS import_status_mappings;
DROP TABLE IF EXISTS slack_emoji_map;
DROP TABLE IF EXISTS import_errors;
DROP TABLE IF EXISTS import_chunks;
DROP TABLE IF EXISTS import_id_map;
DROP TABLE IF EXISTS import_workspace_id_map;
DROP TABLE IF EXISTS import_jobs;
