-- Migration 89: Agent Builder — user-defined, tool-using AI agents.
--
-- An agent generalizes the Workflow Builder (migration 76): instead of a fixed
-- "when X do Y" rule, an agent runs an LLM loop that may call a allow-listed set
-- of tools, bounded by a step budget, acting AS a scoped bot identity tied to
-- its owner. Like workflows, an agent can never do something its owner couldn't
-- do by hand: every tool call re-checks the acting identity's permissions at
-- execution time. No new runtime — triggers ride the existing event bus /
-- mention responder / a ticker; actions reuse the existing AI executors via a
-- shared tool registry.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS ai_agents (
    "id"             uuid PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- Human-facing identity shown wherever the agent acts/posts.
    "name"           varchar NOT NULL,
    "avatar_key"     varchar,                       -- optional object-storage key
    "description"    text,                          -- short tagline for the list

    -- The agent's behavior: system instructions/persona and model preference.
    "instructions"   text NOT NULL DEFAULT '',
    "model_pref"     varchar,                        -- NULL = workspace default

    -- Allow-listed tool names (jsonb array). The runner may ONLY call these,
    -- and each call is still permission-checked at execution time.
    "enabled_tools"  jsonb NOT NULL DEFAULT '[]'::jsonb,

    -- Trigger: how the agent starts. trigger_config holds kind-specific fields
    -- (e.g. cron for schedule, event name + filters for event, keywords for
    -- mention). Open varchar + CHECK so adding kinds is a one-line migration.
    "trigger_type"   varchar NOT NULL DEFAULT 'manual'
        CHECK (trigger_type IN ('manual', 'mention', 'schedule', 'event')),
    "trigger_config" jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- Where the agent may act (jsonb: { "channel_ids": [...], "project_ids": [...] }).
    -- Empty = the owner's full accessible scope.
    "scope"          jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- Safety bound: max LLM/tool steps per run. Hard-capped in the runner too.
    "max_steps"      int NOT NULL DEFAULT 8 CHECK (max_steps > 0 AND max_steps <= 50),

    -- Disabled agents are skipped by triggers but kept for editing.
    "is_active"      boolean NOT NULL DEFAULT true,

    -- Owner: the agent acts AS this user (permissions re-checked per tool call),
    -- so it can never escalate privileges.
    "created_by"     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Bookkeeping.
    "run_count"      bigint NOT NULL DEFAULT 0,
    "last_run_at"    TIMESTAMP WITH TIME ZONE,
    "last_error"     text,

    "created_at"     TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"     TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at"     TIMESTAMP WITH TIME ZONE
);

-- Hot path: trigger workers load "active agents for this trigger type".
CREATE INDEX IF NOT EXISTS idx_ai_agents_active_trigger
    ON ai_agents (trigger_type)
    WHERE is_active = true AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_ai_agents_created_by
    ON ai_agents (created_by)
    WHERE deleted_at IS NULL;

-- Per-run audit: the full transcript of a single agent execution.
CREATE TABLE IF NOT EXISTS ai_agent_runs (
    "id"             uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "agent_id"       uuid NOT NULL REFERENCES ai_agents(id) ON DELETE CASCADE,

    -- Who/what started this run, and the user the run acts as.
    "trigger_source" varchar NOT NULL DEFAULT 'manual',
    "run_as_user_id" uuid REFERENCES users(id) ON DELETE SET NULL,

    -- running | succeeded | failed | stopped (budget/loop guard).
    "status"         varchar NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'succeeded', 'failed', 'stopped')),

    -- Ordered transcript of steps (model messages + tool calls + results).
    "steps"          jsonb NOT NULL DEFAULT '[]'::jsonb,
    "step_count"     int NOT NULL DEFAULT 0,
    "tokens"         bigint NOT NULL DEFAULT 0,
    "result"         text,
    "error"          text,

    "started_at"     TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "ended_at"       TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_ai_agent_runs_agent
    ON ai_agent_runs (agent_id, started_at DESC);
