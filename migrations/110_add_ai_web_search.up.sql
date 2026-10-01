-- Provider-agnostic web search for the AI assistant/agents (Wave 2, Req 3).
-- Admin-configured on the singleton ai_settings row: which provider, its base
-- URL, an (encrypted) API key, and an on/off switch. Default disabled so the
-- capability is off until an admin points it at a provider they run/buy.
ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS web_search_provider    text    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS web_search_base_url     text    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS web_search_api_key_enc  bytea,
    ADD COLUMN IF NOT EXISTS web_search_enabled      boolean NOT NULL DEFAULT false;
