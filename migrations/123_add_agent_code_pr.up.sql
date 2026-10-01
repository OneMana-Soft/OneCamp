-- Migration 123: agent code-PR (config + usage ledger).
--
-- Adds admin-managed configuration for the code-PR agent (see
-- .kiro/specs/agent-code-pr) to the singleton ai_settings row, per-agent daily
-- caps to the agent row, and a usage/audit table that records every coding run.
--
-- The feature is OFF by default (code_pr_enabled=false) and the runner endpoint
-- is unset, so nothing changes for existing installs until an admin explicitly
-- enables it and points it at a coding-capable code-runner sidecar. The runner
-- token is stored encrypted (bytea), like other AI secrets, and never
-- serialized to the FE.
--
-- Budgets: 0 == unlimited (matches the token/sandbox-budget convention). The
-- coding budget dimension is WALL-CLOCK MINUTES + run count. Per-agent caps live
-- on the agent row; per-channel/workspace caps live here.
-- Idempotent.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS code_pr_enabled                boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS code_pr_runner_url             text    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS code_pr_runner_token_enc       bytea,
    ADD COLUMN IF NOT EXISTS code_pr_egress_allowlist        jsonb  NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS code_pr_out_of_scope_policy     text   NOT NULL DEFAULT 'flag_open',
    ADD COLUMN IF NOT EXISTS code_pr_draft_on_red            boolean NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS code_pr_workspace_daily_minutes int    NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS code_pr_workspace_daily_runs    int    NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS code_pr_channel_daily_minutes   int    NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS code_pr_channel_daily_runs      int    NOT NULL DEFAULT 0;

-- Per-agent daily coding caps (mirrors max_daily_tokens / sandbox_daily_*).
ALTER TABLE ai_agents
    ADD COLUMN IF NOT EXISTS code_pr_daily_minutes int NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS code_pr_daily_runs    int NOT NULL DEFAULT 0;

-- Per-run audit + usage ledger. One row per coding run, regardless of outcome.
-- run_id links to ai_agent_runs when the run came from an agent; NULL for an
-- assistant/API-initiated run (actor_id still identifies the user). The full
-- diff lives behind diff_ref (an attachment/object store), not duplicated here;
-- selectors/verifier/judge_verdict are compact JSON for audit + replay.
CREATE TABLE IF NOT EXISTS code_pr_runs (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id            uuid REFERENCES ai_agent_runs(id) ON DELETE SET NULL,
    agent_id          uuid REFERENCES ai_agents(id) ON DELETE SET NULL,
    actor_id          uuid NOT NULL,
    channel_id        uuid,
    repo_owner        text        NOT NULL DEFAULT '',
    repo_name         text        NOT NULL DEFAULT '',
    base_branch       text        NOT NULL DEFAULT '',
    head_branch       text        NOT NULL DEFAULT '',
    whole_repo        boolean     NOT NULL DEFAULT false,
    selectors         jsonb       NOT NULL DEFAULT '[]'::jsonb,
    candidate_count   int         NOT NULL DEFAULT 0,
    diff_ref          text        NOT NULL DEFAULT '',
    diff_files        int         NOT NULL DEFAULT 0,
    diff_added        int         NOT NULL DEFAULT 0,
    diff_removed      int         NOT NULL DEFAULT 0,
    partial_scope     boolean     NOT NULL DEFAULT false,
    verifier          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    had_tests         boolean     NOT NULL DEFAULT false,
    all_passed        boolean     NOT NULL DEFAULT false,
    judge_verdict     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    status            text        NOT NULL DEFAULT '',
    wall_ms           bigint      NOT NULL DEFAULT 0,
    cpu_ms            bigint      NOT NULL DEFAULT 0,
    peak_mem_bytes    bigint      NOT NULL DEFAULT 0,
    verify_iterations int         NOT NULL DEFAULT 0,
    pr_url            text        NOT NULL DEFAULT '',
    message           text        NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT NOW()
);

-- Budgeting queries sum today's usage per tier; index the hot lookups.
CREATE INDEX IF NOT EXISTS idx_code_pr_runs_agent_created   ON code_pr_runs (agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_code_pr_runs_actor_created   ON code_pr_runs (actor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_code_pr_runs_channel_created ON code_pr_runs (channel_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_code_pr_runs_created         ON code_pr_runs (created_at DESC);
