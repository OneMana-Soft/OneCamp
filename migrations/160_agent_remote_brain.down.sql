ALTER TABLE ai_agents
    DROP COLUMN IF EXISTS "agui_endpoint",
    DROP COLUMN IF EXISTS "agui_auth_header",
    DROP COLUMN IF EXISTS "agui_auth_secret_encrypted";
