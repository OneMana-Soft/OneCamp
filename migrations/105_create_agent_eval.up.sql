-- Migration 105: Agent evaluation harness — saved, scored test scenarios.
--
-- Turns the one-shot "test" panel into reusable, scored scenarios so an owner
-- can prove an agent behaves before shipping a change and catch regressions.
-- Additive only; no change to existing agent/run tables.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- A reusable test case for an agent: an input prompt + declarative expectations
-- the run outcome is scored against.
CREATE TABLE IF NOT EXISTS ai_agent_eval_scenarios (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "agent_id"     uuid NOT NULL REFERENCES ai_agents(id) ON DELETE CASCADE,

    "name"         varchar NOT NULL,
    "prompt"       text NOT NULL,

    -- Declarative assertions (jsonb): { must_contain:[], must_not_contain:[],
    -- expected_tools:[], forbidden_tools:[], expected_status:"" }.
    "expectations" jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- Disabled scenarios are kept but skipped by suite runs.
    "is_active"    boolean NOT NULL DEFAULT true,

    "created_by"   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "created_at"   TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"   TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at"   TIMESTAMP WITH TIME ZONE
);

-- List scenarios for an agent (builder surface).
CREATE INDEX IF NOT EXISTS idx_eval_scenarios_agent
    ON ai_agent_eval_scenarios (agent_id)
    WHERE deleted_at IS NULL;

-- One scored execution of a scenario (links to the dry-run it scored).
CREATE TABLE IF NOT EXISTS ai_agent_eval_results (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "scenario_id"  uuid NOT NULL REFERENCES ai_agent_eval_scenarios(id) ON DELETE CASCADE,
    "agent_id"     uuid NOT NULL REFERENCES ai_agents(id) ON DELETE CASCADE,
    "run_id"       uuid REFERENCES ai_agent_runs(id) ON DELETE SET NULL,

    "passed"       boolean NOT NULL DEFAULT false,
    "inconclusive" boolean NOT NULL DEFAULT false,
    "score"        int NOT NULL DEFAULT 0,        -- 0..100
    -- Per-expectation detail (jsonb array of {kind,target,passed,reason}) +
    -- an inconclusive reason when applicable.
    "checks"       jsonb NOT NULL DEFAULT '[]'::jsonb,
    "reason"       text,

    "created_by"   uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"   TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Trend / latest-result lookups per scenario and per agent.
CREATE INDEX IF NOT EXISTS idx_eval_results_scenario
    ON ai_agent_eval_results (scenario_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_eval_results_agent
    ON ai_agent_eval_results (agent_id, created_at DESC);
