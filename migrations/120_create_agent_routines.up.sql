-- Migration 120: agent routines (conversational standing work).
--
-- A routine is a named recurring job a user hands an AI agent from chat
-- ("every weekday at 9am summarize this channel"): the agent re-runs the
-- routine's prompt on a schedule in the channel/group it was created in, using
-- the same owner-identity, permission-checked run path as an interactive
-- mention. This is OneCamp's answer to Claude Tag's per-channel routines, but
-- generic: many routines per agent per surface, each independently enable-able.
--
-- Recurrence reuses the existing Scheduler RRULE-lite engine (business/Scheduler
-- recurrence.go): `recurrence` is a rule like FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR
-- and `at_minute_utc` is minutes past UTC midnight for the fire time. A NULL
-- channel_id + NULL group_id means "no single surface" (a manual/DM-less routine
-- that only makes sense with a surface, so the tool requires one).
--
-- Idempotent.

CREATE TABLE IF NOT EXISTS agent_routines (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id      uuid NOT NULL REFERENCES ai_agents(id) ON DELETE CASCADE,
    -- The human who created the routine (app user uuid). The run still executes
    -- as the agent's owner; created_by is for attribution + who may cancel it.
    created_by    uuid NOT NULL,
    -- Exactly one of channel_id / group_id identifies the surface it posts to.
    channel_id    uuid,
    group_id      text NOT NULL DEFAULT '',
    name          text NOT NULL DEFAULT '',
    prompt        text NOT NULL,
    recurrence    text NOT NULL,             -- RRULE-lite, e.g. FREQ=DAILY
    at_minute_utc int  NOT NULL DEFAULT 0,   -- fire time, minutes past UTC midnight
    enabled       boolean NOT NULL DEFAULT true,
    last_run_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT NOW(),
    updated_at    timestamptz NOT NULL DEFAULT NOW(),
    deleted_at    timestamptz
);

-- The dispatcher scans enabled, non-deleted routines each tick.
CREATE INDEX IF NOT EXISTS idx_agent_routines_due
    ON agent_routines (enabled)
    WHERE deleted_at IS NULL;

-- List routines for one agent, and for one channel (the two UI/tool queries).
CREATE INDEX IF NOT EXISTS idx_agent_routines_agent
    ON agent_routines (agent_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_agent_routines_channel
    ON agent_routines (channel_id) WHERE deleted_at IS NULL;
