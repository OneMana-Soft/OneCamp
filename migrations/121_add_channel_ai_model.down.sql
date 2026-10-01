-- Rollback migration 121: drop the per-channel default AI model.
ALTER TABLE channels DROP COLUMN IF EXISTS ai_model_id;
