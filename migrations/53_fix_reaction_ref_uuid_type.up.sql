-- Migration 53: Fix reaction_uuid type mismatch
-- Background: Dgraph returns reaction UIDs as strings (e.g. "0x1234"),
-- not UUIDs.  Migration 52 incorrectly typed reaction_uuid as uuid,
-- causing all inserts to fail silently and breaking dedup + deletion.

ALTER TABLE github_reaction_refs ALTER COLUMN reaction_uuid TYPE TEXT;

-- Also make it nullable since inbound reactions don't have a OneCamp
-- reaction UUID until after creation (when we get the Dgraph UID).
ALTER TABLE github_reaction_refs ALTER COLUMN reaction_uuid DROP NOT NULL;
