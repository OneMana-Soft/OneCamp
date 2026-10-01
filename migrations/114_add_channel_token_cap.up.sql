-- Migration 114: per-channel daily AI token cap (Claude-Tag-style channel-level
-- cost control).
--
-- Caps the TOTAL AI spend incurred within a channel per UTC day — the shared
-- coworker's @mention replies plus every agent that works there — independent
-- of any per-agent or per-user cap. The runner/coworker meter channel spend on
-- the per-channel budget dimension (services/AI budget.go) and decline once the
-- cap is hit, telling the requester in-thread (matching Claude Tag, which
-- declines over-limit work rather than truncating it).
--
-- Additive + defaulted: 0 means "no per-channel cap" (only the workspace cap
-- applies), so every existing channel is unchanged.

ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS max_daily_ai_tokens int NOT NULL DEFAULT 0
        CHECK (max_daily_ai_tokens >= 0);
