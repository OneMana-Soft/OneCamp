DROP TABLE IF EXISTS code_pr_runs;

ALTER TABLE ai_agents
    DROP COLUMN IF EXISTS code_pr_daily_minutes,
    DROP COLUMN IF EXISTS code_pr_daily_runs;

ALTER TABLE ai_settings
    DROP COLUMN IF EXISTS code_pr_enabled,
    DROP COLUMN IF EXISTS code_pr_runner_url,
    DROP COLUMN IF EXISTS code_pr_runner_token_enc,
    DROP COLUMN IF EXISTS code_pr_egress_allowlist,
    DROP COLUMN IF EXISTS code_pr_out_of_scope_policy,
    DROP COLUMN IF EXISTS code_pr_draft_on_red,
    DROP COLUMN IF EXISTS code_pr_workspace_daily_minutes,
    DROP COLUMN IF EXISTS code_pr_workspace_daily_runs,
    DROP COLUMN IF EXISTS code_pr_channel_daily_minutes,
    DROP COLUMN IF EXISTS code_pr_channel_daily_runs;
