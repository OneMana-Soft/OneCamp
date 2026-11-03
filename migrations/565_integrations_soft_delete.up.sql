-- V-184 head 5b: an integration disconnected through its domain (the org's
-- GitHub, a person's Google Calendar) is kept, marked by who disconnected it
-- and when, its credentials cleared (the owner's rule that every delete of a
-- business record is soft). deleted_by is a plain uuid, no foreign key, as
-- 555's. No backfill and no index here: every read says deleted_at IS NULL,
-- and 566 makes an integration unique among connected ones only, so connecting
-- again makes a new row beside the one kept.
ALTER TABLE integrations ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
