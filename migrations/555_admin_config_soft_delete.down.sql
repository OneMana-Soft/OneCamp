-- The rows taken away go before their marks: 560's down has taken them already
-- (it puts the full uniques back), so these find none unless it never ran. A
-- provider's delete cascades to its models and sets the settings that named
-- it to NULL, as before V-184 head 4b.
DELETE FROM slack_bridge_links WHERE deleted_at IS NOT NULL;
DELETE FROM ai_authorized_models WHERE deleted_at IS NOT NULL;
DELETE FROM ai_providers WHERE deleted_at IS NOT NULL;
ALTER TABLE slack_bridge_links DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE ai_authorized_models DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
ALTER TABLE ai_providers DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
