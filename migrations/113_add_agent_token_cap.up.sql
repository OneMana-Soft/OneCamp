-- Migration 113: per-agent daily token cap.
--
-- Until now an AI teammate's autonomous spend was metered against its OWNER's
-- personal daily quota (WithActor(owner)), so a busy agent ate the owner's
-- interactive budget and there was no way to bound one teammate independently.
-- This adds an explicit per-agent daily cap. The runner meters agent spend on a
-- dedicated per-agent budget dimension (services/AI budget.go) instead of the
-- owner's seat — matching how Claude Tag bills channel/teammate work to the org,
-- not individual seats — and refuses further work for the day once the cap is
-- hit (recorded as a clean stopped-with-reason run, conveyed in-thread).
--
-- Additive + defaulted: 0 means "no per-agent cap" (only the workspace cap
-- applies), so every existing agent is unchanged.

ALTER TABLE ai_agents
    ADD COLUMN IF NOT EXISTS max_daily_tokens int NOT NULL DEFAULT 0
        CHECK (max_daily_tokens >= 0);
