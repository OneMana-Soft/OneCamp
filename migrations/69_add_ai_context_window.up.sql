-- Migration 69: admin-configurable AI context window.
--
-- The model's context window (Ollama num_ctx, or the effective input window
-- for a cloud model) was previously a boot-time env var (OLLAMA_NUM_CTX /
-- AI_CONTEXT_TOKENS) while the MODEL SELECTION itself is already runtime/
-- DB-managed (ai_settings). That mismatch meant an admin who switched to a
-- larger-window model couldn't actually use the bigger window without
-- editing env and restarting.
--
-- This column moves the window into the admin-managed settings so it travels
-- with the model selection and hot-reloads with it. It flows into BOTH the
-- prompt token budget AND the provider's actual num_ctx, so the budget can
-- never diverge from what the model is run with.
--
-- 0 == "use the env/default" (OLLAMA_NUM_CTX or 8192), so existing installs
-- are unchanged until an admin sets a value. Idempotent.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS "context_window_tokens" int NOT NULL DEFAULT 0;
