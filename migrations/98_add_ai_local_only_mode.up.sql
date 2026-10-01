-- Migration 98: AI local-only mode (data-residency guarantee).
--
-- When enabled, the AI provider dial guard refuses any non-local model
-- endpoint, so no workspace content can leave to a cloud model. Off by default
-- to preserve existing behavior; an admin opts in (or pins it via the
-- AI_LOCAL_ONLY_MODE env var).
ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS "local_only_mode" boolean NOT NULL DEFAULT false;
