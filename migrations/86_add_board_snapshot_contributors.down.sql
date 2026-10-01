-- Rollback migration 86.
ALTER TABLE board_snapshots DROP COLUMN IF EXISTS contributor_uuids;
