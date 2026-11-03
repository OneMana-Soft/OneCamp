-- V-184 head 4a (the owner's rule: every delete is soft): five settings taken
-- away are marked, when and by whom, instead of removed, so who set them, who
-- took them away and when stay on record: a project's status rule, its billing
-- and a person's own rate on it, a channel's AI memory exclusion and a doc's
-- consent to workspace exports. Every read of these tables takes live rows only
-- (models/postgres/SoftDelete.Live). Setting one again brings its row back, as
-- each is keyed by what it is about (ON CONFLICT ... deleted_at = NULL).
-- Clearing a project's billing marks its people's rates too, as the delete's
-- cascade took them before. deleted_by is a plain uuid, no FK, as 550's.
ALTER TABLE status_rules ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE doc_export_consents ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE ai_memory_exclusions ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE project_billing ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE project_person_rates ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
