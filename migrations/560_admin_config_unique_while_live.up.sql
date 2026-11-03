-- V-184 head 4b: the four full uniques give way to 556-559's partial ones,
-- which hold the same names among live rows only. A full unique counts a row
-- taken away (kept, marked), so adding the same provider, model or link again
-- would collide with it. Dropping builds nothing, so it runs in this
-- migration's transaction.
DROP INDEX IF EXISTS uq_ai_providers_label;
ALTER TABLE ai_authorized_models DROP CONSTRAINT IF EXISTS ai_authorized_models_provider_id_model_key;
ALTER TABLE slack_bridge_links DROP CONSTRAINT IF EXISTS slack_bridge_links_slack_channel_id_key, DROP CONSTRAINT IF EXISTS slack_bridge_links_channel_uuid_key;
