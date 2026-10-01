-- Persist a delegation chain's lineage on the durable job.
--
-- Why: a durable job crosses a process boundary — enqueued by one goroutine,
-- executed later, possibly on another node — so the in-memory context carrying the
-- lineage is gone by the time the job runs. Until now the worker REBUILT lineage
-- from triggered_by: it recovered the originating person (which is what makes a hop
-- authorised and auditable) but not the agent chain, so it had to assume hop 1 with
-- a single-agent chain.
--
-- That assumption is safe but weaker than it should be. Cycle detection works by
-- IDENTITY — refusing an agent already in the chain — and a rebuilt chain has
-- forgotten who acted, so a durable chain could only be bounded by the hop budget.
-- Two agents relaying through durable jobs would each see themselves as starting
-- fresh, exactly the case the identity check exists to catch. With the lineage
-- stored, a durable hop gets the same protection as a synchronous one, and raising
-- the hop budget stops being disproportionately risky.
--
-- Defaults are the "not part of a chain" reading, which is what every existing row
-- is and what a human-triggered job should be, so the backfill is the default and
-- no data migration is needed.

ALTER TABLE ai_agent_tasks
    -- 0 = a person asked directly. >0 = this job is the Nth agent hand-off.
    ADD COLUMN IF NOT EXISTS delegation_hop integer NOT NULL DEFAULT 0,

    -- The agent ids already in this walk, in order. jsonb rather than uuid[] to
    -- match the existing `messages` column's storage choice on this table, so the
    -- row has one array convention rather than two.
    ADD COLUMN IF NOT EXISTS delegation_chain jsonb NOT NULL DEFAULT '[]'::jsonb;

-- A negative hop would hand out unlimited budget (the guard compares hop against a
-- ceiling), and an object where an array is expected would fail to decode into the
-- chain. Both are caller bugs rather than states to tolerate, so they are refused
-- at the boundary that outlives any single caller.
ALTER TABLE ai_agent_tasks
    ADD CONSTRAINT ai_agent_tasks_delegation_hop_non_negative
        CHECK (delegation_hop >= 0),
    ADD CONSTRAINT ai_agent_tasks_delegation_chain_is_array
        CHECK (jsonb_typeof(delegation_chain) = 'array');
