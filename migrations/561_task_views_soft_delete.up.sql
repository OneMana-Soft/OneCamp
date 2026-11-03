-- V-184 head 5a: a saved task view its person deletes is kept, marked by them
-- and when (the owner's rule that every delete of a business record is soft).
-- deleted_by is a plain uuid, no foreign key, as 555's. No backfill and no
-- index here: every read says deleted_at IS NULL, and 562 makes a person's
-- view names unique among live views only, so a deleted view's name saved
-- again is a new view beside the one deleted.
ALTER TABLE task_views ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
