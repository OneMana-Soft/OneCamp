-- Migration 360: an app is an identity (platform P-API, design/platform/
-- API_CONTRACT.md §3.1; the working number is the platform's, 360-379, and the
-- integrator gives it the next free number at merge).
--
-- An app installed in the workspace (apps, migration 73) gets what a member of
-- it would have: its own user to write as (a bot, so its messages are its own
-- and notify like people's), the scopes its tokens may carry at most, and an
-- audience: the places it was added to, which it may read and write, and
-- nowhere else. Its credentials are API tokens (api_tokens) bound to it by
-- app_id, as agent tokens are bound by agent_id (migration 138): an identity
-- holds several credentials, rotated in place, and turning the app off
-- (apps.is_enabled) stops every one of them on the next call.
--
-- Postgres 12: no gen_random_uuid here; ids come from Go or uuid_generate_v4.

ALTER TABLE apps ADD COLUMN IF NOT EXISTS bot_user_id uuid REFERENCES users(id);
-- What the app's tokens may ask for, at most: ["messages:write", "tasks:read"].
ALTER TABLE apps ADD COLUMN IF NOT EXISTS scopes jsonb NOT NULL DEFAULT '[]'::jsonb;
-- Where it may read and write: {"places": [{"kind": "channel", "id": "…"}], "public": false}.
ALTER TABLE apps ADD COLUMN IF NOT EXISTS audience jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS app_id uuid REFERENCES apps(id) ON DELETE CASCADE;

-- An app's tokens, for its page and for revoking them all at once.
CREATE INDEX IF NOT EXISTS idx_api_tokens_app
    ON api_tokens (app_id)
    WHERE app_id IS NOT NULL AND revoked_at IS NULL;
