-- Reverting returns durable chains to rebuilding lineage from triggered_by: the
-- originating person is still recovered (so hops stay authorised and auditable) and
-- the chain is assumed to be this agent alone, which is the conservative reading the
-- code already falls back to when these columns hold their defaults. So a rollback
-- loses cycle-detection strength, not correctness.
ALTER TABLE ai_agent_tasks
    DROP CONSTRAINT IF EXISTS ai_agent_tasks_delegation_hop_non_negative,
    DROP CONSTRAINT IF EXISTS ai_agent_tasks_delegation_chain_is_array;

ALTER TABLE ai_agent_tasks
    DROP COLUMN IF EXISTS delegation_hop,
    DROP COLUMN IF EXISTS delegation_chain;
