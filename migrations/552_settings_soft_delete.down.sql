-- The schema before V-184 head 4a has no settings taken away: taking one away
-- removed it. Rows marked since go as they would have then, first, so none
-- comes back live when the columns go (a cleared billing's rates with it).
DELETE FROM project_person_rates WHERE deleted_at IS NOT NULL;
DELETE FROM project_billing WHERE deleted_at IS NOT NULL;
DELETE FROM ai_memory_exclusions WHERE deleted_at IS NOT NULL;
DELETE FROM doc_export_consents WHERE deleted_at IS NOT NULL;
DELETE FROM status_rules WHERE deleted_at IS NOT NULL;
ALTER TABLE project_person_rates DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE project_billing DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE ai_memory_exclusions DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE doc_export_consents DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE status_rules DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
