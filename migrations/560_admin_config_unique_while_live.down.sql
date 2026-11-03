-- The full uniques count every row, so the rows taken away go first: one added
-- again beside them would collide. A provider's delete cascades to its models
-- and sets the settings that named it to NULL, as before V-184 head 4b.
DELETE FROM slack_bridge_links WHERE deleted_at IS NOT NULL;
DELETE FROM ai_authorized_models WHERE deleted_at IS NOT NULL;
DELETE FROM ai_providers WHERE deleted_at IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_ai_providers_label ON ai_providers (lower(label));
ALTER TABLE ai_authorized_models ADD CONSTRAINT ai_authorized_models_provider_id_model_key UNIQUE (provider_id, model);
ALTER TABLE slack_bridge_links ADD CONSTRAINT slack_bridge_links_slack_channel_id_key UNIQUE (slack_channel_id), ADD CONSTRAINT slack_bridge_links_channel_uuid_key UNIQUE (channel_uuid);
