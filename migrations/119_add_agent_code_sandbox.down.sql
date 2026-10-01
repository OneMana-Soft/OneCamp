DROP TABLE IF EXISTS sandbox_runs;

ALTER TABLE ai_agents
    DROP COLUMN IF EXISTS sandbox_daily_seconds,
    DROP COLUMN IF EXISTS sandbox_daily_runs;

ALTER TABLE ai_settings
    DROP COLUMN IF EXISTS sandbox_enabled,
    DROP COLUMN IF EXISTS sandbox_runner_url,
    DROP COLUMN IF EXISTS sandbox_runner_token_enc,
    DROP COLUMN IF EXISTS sandbox_image_digest,
    DROP COLUMN IF EXISTS sandbox_workspace_daily_seconds,
    DROP COLUMN IF EXISTS sandbox_workspace_daily_runs,
    DROP COLUMN IF EXISTS sandbox_channel_daily_seconds,
    DROP COLUMN IF EXISTS sandbox_channel_daily_runs;
