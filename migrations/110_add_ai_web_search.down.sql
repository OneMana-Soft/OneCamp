ALTER TABLE ai_settings
    DROP COLUMN IF EXISTS web_search_provider,
    DROP COLUMN IF EXISTS web_search_base_url,
    DROP COLUMN IF EXISTS web_search_api_key_enc,
    DROP COLUMN IF EXISTS web_search_enabled;
