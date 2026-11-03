-- The integrations disconnected go before their marks: 567's down has taken
-- them already (it puts the full unique back), so this finds none unless it
-- never ran.
DELETE FROM integrations WHERE deleted_at IS NOT NULL;
ALTER TABLE integrations DROP COLUMN IF EXISTS "deleted_at", DROP COLUMN IF EXISTS "deleted_by";
