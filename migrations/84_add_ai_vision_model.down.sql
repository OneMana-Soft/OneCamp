-- Revert migration 84.
ALTER TABLE ai_settings DROP COLUMN IF EXISTS vision_model;
ALTER TABLE ai_settings DROP COLUMN IF EXISTS vision_provider_id;
