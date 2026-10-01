-- Revert migration 79.
ALTER TABLE channels DROP COLUMN IF EXISTS post_policy;
