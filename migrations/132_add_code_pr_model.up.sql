-- Migration 132: optional dedicated model for the code-PR coding runner.
--
-- The code-runner sidecar calls back to the server's LLM proxy for every
-- completion in its edit/verify loop. By default that used the workspace CHAT
-- model, so a coding run competed for (and was starved by) the chat model's
-- provider quota/daily cap. These columns let an admin point code runs at a
-- SEPARATE provider+model (e.g. a higher-tier or uncapped endpoint) so coding
-- isn't blocked when chat is rate-limited. Unset (NULL / '') = fall back to the
-- chat model, preserving today's behavior. Idempotent.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS code_pr_chat_provider_id uuid REFERENCES ai_providers(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS code_pr_chat_model       text NOT NULL DEFAULT '';
