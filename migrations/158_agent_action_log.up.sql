-- What the agent was about to do, written BEFORE it does it.
--
-- THE GAP THIS CLOSES. The evidence pack says so itself: "A hash chain is
-- evidence of integrity, not of completeness, and no export can turn one into
-- the other." That was exactly true. A run's tool calls accumulated in memory
-- and reached the database only when the run FINISHED, so a process that died
-- mid-run left actions that had really happened -- a message sent, an issue
-- commented on, a task created -- with nothing anywhere recording them. The log
-- could be proven unaltered and could not be proven complete.
--
-- Writing the intent first inverts which way the uncertainty falls. After a
-- crash the log may claim an action that never took effect, and can no longer
-- miss one that did. Over-recording is the safe direction: an auditor reading
-- "we intended this and cannot confirm the outcome" is being told the truth,
-- where silence was not.
--
-- outcome IS NULL is therefore the meaningful state, not an error: intent was
-- recorded and the process never came back to say what happened. Those rows are
-- what the evidence pack now reports, and they are the residual uncertainty
-- stated precisely rather than a caveat covering everything.
--
-- EFFECTING CALLS ONLY. A read that went unrecorded cannot mean an unrecorded
-- change, and paying a synchronous write for every lookup would buy nothing. The
-- completeness claim is about effects, and says so.
--
-- The parameters are stored as a DIGEST, never as themselves. Proving which call
-- was made does not require keeping the message body it carried, and the pack
-- already fingerprints instructions the same way.
CREATE TABLE IF NOT EXISTS ai_agent_action_log (
    "id"            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- Not a foreign key to ai_agent_runs on purpose: CreateRun can fail and the
    -- run still happens, and a record that refuses to exist because its parent
    -- row is missing is the failure this table exists to prevent.
    "run_id"        uuid,
    "agent_id"      uuid NOT NULL,
    "run_as_user_id" uuid,

    "tool_name"     varchar NOT NULL,
    -- SHA-256 of the canonical parameters. Identifies the call without keeping
    -- its contents.
    "params_digest" varchar NOT NULL,

    -- Written before the attempt. The whole point of the table.
    "intent_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    -- NULL means the process did not return after recording intent.
    "outcome"       varchar CHECK (outcome IN ('ok', 'error', 'skipped')),
    "outcome_at"    TIMESTAMP WITH TIME ZONE,
    "error"         text
);

-- The evidence pack's query: unresolved actions in a window, newest first.
CREATE INDEX IF NOT EXISTS ai_agent_action_log_unresolved_idx
    ON ai_agent_action_log (intent_at DESC)
    WHERE outcome IS NULL;

-- Per-run assembly for the run transcript.
CREATE INDEX IF NOT EXISTS ai_agent_action_log_run_idx
    ON ai_agent_action_log (run_id, intent_at);
