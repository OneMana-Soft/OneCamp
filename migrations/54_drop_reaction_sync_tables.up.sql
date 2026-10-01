-- Migration 54: Drop reaction sync tables
-- Background: Bidirectional GitHub reaction sync has been removed entirely.
-- Reactions remain as a local OneCamp feature only.

-- Drop the reaction reference tracking table
DROP TABLE IF EXISTS github_reaction_refs CASCADE;

-- Drop the emoji mapping table (only used for GitHub reaction sync)
DROP TABLE IF EXISTS emoji_sync_map CASCADE;

-- Clean up any pending reaction sync queue items
DELETE FROM github_sync_queue WHERE sync_type = 'reaction';
