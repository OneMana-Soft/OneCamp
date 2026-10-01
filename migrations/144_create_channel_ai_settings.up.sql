-- Migration 144: per-channel opt-in for the weekly channel report.
--
-- team_report_enabled in ai_settings is a single org-wide switch, and turning
-- it on made the agent discover EVERY active channel and post into all of them
-- (up to 100 per run) with no way for a channel to decline. An admin who wanted
-- the report in two engineering channels had to accept it in the exec and HR
-- channels too, so in practice it stayed off and nobody used the feature.
--
-- The org switch now sets the ceiling and this table records the opt-in beneath
-- it. Both must be true for a report to post, so the org can still turn the
-- whole thing off in one place and a channel can never override that.
--
-- An ABSENT ROW MEANS DISABLED. That is what makes the default off without a
-- backfill, and it means enabling the org switch changes nothing on its own.
-- Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS channel_ai_settings (
    channel_uuid        uuid PRIMARY KEY,
    team_report_enabled boolean NOT NULL DEFAULT false,
    updated_by          uuid,
    updated_at          timestamptz NOT NULL DEFAULT NOW()
);

-- The agent's per-run question is "which channels opted in", never "what did
-- this one channel choose", so the index serves the read that runs weekly
-- across the whole workspace.
CREATE INDEX IF NOT EXISTS idx_channel_ai_settings_team_report
    ON channel_ai_settings (channel_uuid)
    WHERE team_report_enabled;
