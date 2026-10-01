-- Migration 147: attribute a proposed action to the agent (and run) that
-- proposed it.
--
-- WHY. Approving or denying a proposal is the highest quality outcome signal
-- the product produces: a decision a person makes anyway, about real work, on
-- the way to getting something done. It is not a rating widget nobody fills in.
-- The record was already durable and already carried who resolved it and when.
-- What it never carried was WHOSE proposal it was, so the signal could not be
-- attributed to anything and was lost.
--
-- The agent is technically recoverable today from (surface_type = 'agent',
-- surface_id = <agent uuid>), which is a foreign key kept in a varchar with no
-- constraint behind it. A real column is indexable, enforced, and survives the
-- surface convention changing. The run id was never recoverable at all.
--
-- Both nullable: the assistant, the coworker and MCP servers all propose
-- actions too, and those have no agent behind them.
ALTER TABLE ai_pending_actions
    ADD COLUMN IF NOT EXISTS "agent_id" uuid REFERENCES ai_agents(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS "run_id"   uuid REFERENCES ai_agent_runs(id) ON DELETE SET NULL;

-- The aggregation this exists for: accepted vs denied per agent. Partial,
-- because the majority of rows have no agent and would only bloat the index.
CREATE INDEX IF NOT EXISTS idx_ai_pending_actions_agent_outcome
    ON ai_pending_actions (agent_id, status)
    WHERE agent_id IS NOT NULL;

-- Per-run lookup, so a run can show what became of what it proposed.
CREATE INDEX IF NOT EXISTS idx_ai_pending_actions_run
    ON ai_pending_actions (run_id)
    WHERE run_id IS NOT NULL;
