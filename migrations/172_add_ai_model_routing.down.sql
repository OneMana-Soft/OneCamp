ALTER TABLE ai_settings DROP CONSTRAINT IF EXISTS ai_settings_model_routing_is_object;
ALTER TABLE ai_settings DROP COLUMN IF EXISTS model_routing;
