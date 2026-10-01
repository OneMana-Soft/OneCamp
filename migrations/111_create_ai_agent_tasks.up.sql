-- Migration 111: durable AI agent tasks — the resumable job queue behind
-- "assign a task to an AI teammate".
--
-- An agent RUN (ai_agent_runs) is a single, in-process, bounded tool-loop; it
-- is lost if the process restarts mid-run and cannot wait hours for a human and
-- resume. This table makes the JOB durable: when work is handed to an agent
-- (today: a project task is assigned to the agent's bot principal), one row is
-- enqueued here and a worker drives it to a terminal state, surviving restarts,
-- retrying transient failures with backoff, and reclaiming a job whose worker
-- died mid-run. Each attempt still executes through the proven runner
-- (RunAgent) AS the owner with per-tool permission re-checks — no new
-- privilege path, no parallel model runtime.
--
-- State lifecycle:
--   queued     -> running                    (a worker leased it)
--   running    -> done | failed              (terminal: succeeded / gave up)
--   running    -> queued                     (transient error -> backoff retry,
--                                             or lease expired after a crash)
--   running    -> awaiting_input             (token budget / needs a human;
--                                             resumes when re-queued)
-- The queued -> running transition is a guarded UPDATE (FOR UPDATE SKIP LOCKED
-- + lease token) so two workers can never run the same job at once.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS ai_agent_tasks (
    "id"             uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "agent_id"       uuid NOT NULL REFERENCES ai_agents(id) ON DELETE CASCADE,

    -- What handed work to the agent, so the worker can post progress back to
    -- the right surface and dedupe. v1: 'task_assignment' (source_id = the
    -- project task uuid). Open varchar so adding sources is a one-line change.
    "source_type"    varchar NOT NULL DEFAULT 'task_assignment',
    "source_id"      varchar NOT NULL DEFAULT '',

    -- The run input (synthesized from the source) and the user the run acts as
    -- (the agent's owner; permissions re-checked per tool call).
    "prompt"         text NOT NULL DEFAULT '',
    "run_as_user_id" uuid REFERENCES users(id) ON DELETE SET NULL,

    -- queued | running | awaiting_input | done | failed
    "state"          varchar NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'running', 'awaiting_input', 'done', 'failed')),

    -- Retry bookkeeping. attempt counts started runs; max_attempts caps them.
    -- next_attempt_at gates when a backed-off job becomes runnable again.
    "attempt"        int NOT NULL DEFAULT 0,
    "max_attempts"   int NOT NULL DEFAULT 3,
    "next_attempt_at" timestamptz NOT NULL DEFAULT now(),

    -- Crash-safety lease: a worker stamps lease_token + lease_expires_at when it
    -- claims a job; a job stuck 'running' past its lease is reclaimable.
    "lease_token"    uuid,
    "lease_expires_at" timestamptz,

    -- Last run linked for the audit transcript, last error for transparency.
    "last_run_id"    uuid REFERENCES ai_agent_runs(id) ON DELETE SET NULL,
    "last_error"     text,
    "result"         text,

    "created_at"     timestamptz NOT NULL DEFAULT now(),
    "updated_at"     timestamptz NOT NULL DEFAULT now(),
    "ended_at"       timestamptz
);

-- Worker hot path: the next runnable jobs (queued/awaiting that are due),
-- oldest first. Partial index keeps it tiny (terminal rows excluded).
CREATE INDEX IF NOT EXISTS idx_ai_agent_tasks_runnable
    ON ai_agent_tasks (next_attempt_at)
    WHERE state IN ('queued', 'awaiting_input');

-- Reclaim path: jobs stuck 'running' past their lease (a crashed worker).
CREATE INDEX IF NOT EXISTS idx_ai_agent_tasks_lease
    ON ai_agent_tasks (lease_expires_at)
    WHERE state = 'running';

-- Per-source dedupe + lookup (e.g. "is this task already queued for an agent").
CREATE INDEX IF NOT EXISTS idx_ai_agent_tasks_source
    ON ai_agent_tasks (source_type, source_id);

-- Per-agent history.
CREATE INDEX IF NOT EXISTS idx_ai_agent_tasks_agent
    ON ai_agent_tasks (agent_id, created_at DESC);

-- One open (non-terminal) job per (source, agent) so re-assigning or repeated
-- events don't stack duplicate jobs for the same work.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_agent_tasks_open_source
    ON ai_agent_tasks (source_type, source_id, agent_id)
    WHERE state IN ('queued', 'running', 'awaiting_input');
