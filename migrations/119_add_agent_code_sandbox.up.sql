-- Migration 119: agent execution sandbox (config + usage ledger).
--
-- Adds admin-managed configuration for the bounded, network-less code-execution
-- sandbox (see .kiro/specs/agent-code-sandbox) to the singleton ai_settings
-- row, plus a usage table that records every run for audit and budgeting.
--
-- The feature is OFF by default (sandbox_enabled=false) and the runner endpoint
-- is unset, so nothing changes for existing installs until an admin explicitly
-- enables it and points it at a code-runner sidecar. The runner token is stored
-- encrypted (bytea), like other AI secrets, and never serialized to the FE.
--
-- Budgets: 0 == unlimited (matches the token-budget convention). Per-agent caps
-- live on the agent row (below); per-channel/workspace caps live here.
-- Idempotent.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS sandbox_enabled            boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS sandbox_runner_url         text    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS sandbox_runner_token_enc   bytea,
    ADD COLUMN IF NOT EXISTS sandbox_image_digest       text    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS sandbox_workspace_daily_seconds int NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sandbox_workspace_daily_runs    int NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sandbox_channel_daily_seconds   int NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sandbox_channel_daily_runs      int NOT NULL DEFAULT 0;

-- Per-agent daily sandbox caps (mirrors max_daily_tokens on the agent row).
ALTER TABLE ai_agents
    ADD COLUMN IF NOT EXISTS sandbox_daily_seconds int NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sandbox_daily_runs    int NOT NULL DEFAULT 0;

-- Per-run audit + usage ledger. One row per sandbox run, regardless of outcome.
-- run_id links to ai_agent_runs when the run came from an agent; it is NULL for
-- an assistant-initiated run (actor_id still identifies the user). code_sha256
-- is the hash of the executed code (the full code lives in the agent transcript,
-- not duplicated here). input_refs is a JSON array of resolved input descriptors
-- (ids/queries — never the raw data). status is the typed RunStatus.
CREATE TABLE IF NOT EXISTS sandbox_runs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          uuid REFERENCES ai_agent_runs(id) ON DELETE SET NULL,
    agent_id        uuid REFERENCES ai_agents(id) ON DELETE SET NULL,
    actor_id        uuid NOT NULL,
    channel_id      uuid,
    code_sha256     text        NOT NULL DEFAULT '',
    input_refs      jsonb       NOT NULL DEFAULT '[]'::jsonb,
    status          text        NOT NULL DEFAULT '',
    wall_ms         bigint      NOT NULL DEFAULT 0,
    cpu_ms          bigint      NOT NULL DEFAULT 0,
    peak_mem_bytes  bigint      NOT NULL DEFAULT 0,
    artifact_count  int         NOT NULL DEFAULT 0,
    artifact_bytes  bigint      NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT NOW()
);

-- Budgeting queries sum today's usage per tier; index the hot lookups.
CREATE INDEX IF NOT EXISTS idx_sandbox_runs_agent_created  ON sandbox_runs (agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sandbox_runs_actor_created  ON sandbox_runs (actor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sandbox_runs_channel_created ON sandbox_runs (channel_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sandbox_runs_created        ON sandbox_runs (created_at DESC);
