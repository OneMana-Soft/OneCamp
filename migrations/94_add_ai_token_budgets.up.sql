-- Migration 94: admin-configurable daily AI token budgets.
--
-- The two daily token caps (workspace-wide and per-user) were previously
-- boot-time env vars (AI_WORKSPACE_DAILY_TOKEN_BUDGET /
-- AI_USER_DAILY_TOKEN_BUDGET), while every other AI knob (model selection,
-- rate limit, context window) is already DB-managed and hot-reloads. That
-- mismatch meant an admin had to edit env and redeploy to change a cap.
--
-- These columns move the caps into the admin-managed settings so they can be
-- edited from the AI panel and take effect immediately on the next request,
-- without a restart. They gate ALL AI usage (assistant, Board AI, agents,
-- tables, workflows, ...) at the provider layer.
--
-- 0 == unlimited, so existing installs are unchanged until an admin sets a
-- value. The env vars remain a boot fallback. Idempotent.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS "workspace_daily_token_budget" int NOT NULL DEFAULT 0;
ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS "user_daily_token_budget" int NOT NULL DEFAULT 0;
