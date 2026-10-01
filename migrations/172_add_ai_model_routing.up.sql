-- Migration 172: which model does which kind of background work.
--
-- Everything that is not a person's own chat or an agent's run (catch-ups,
-- channel summaries, briefings, team reports, nudges, meeting recaps, memory
-- extraction) used the workspace's one default model. A workspace whose
-- default is a large cloud model paid that price for every catch-up, and one
-- whose default is a small local model got thin meeting recaps. Routing lets
-- an admin send each kind of work to a model on the allowlist.
--
-- {"<purpose>": {"provider_id": "<uuid>", "model": "<name>"}}. A purpose that
-- is absent uses the workspace default, so the empty object is today's
-- behaviour.
ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS model_routing jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE ai_settings
    ADD CONSTRAINT ai_settings_model_routing_is_object
        CHECK (jsonb_typeof(model_routing) = 'object');
