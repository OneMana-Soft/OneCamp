-- Migration 72: Durable scheduler for time-delayed work (/remind, scheduled
-- messages, recurring digests). This is the missing primitive — until now the
-- only "reminder" was a calendar event (set_reminder), which does not DELIVER
-- a nudge. scheduled_jobs is a claim-based queue safe across restarts and
-- multiple replicas (SELECT ... FOR UPDATE SKIP LOCKED).

CREATE TABLE IF NOT EXISTS scheduled_jobs (
    "id"           uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    -- job_type drives which handler runs: reminder | scheduled_message | recurring_digest
    "job_type"     varchar NOT NULL,
    -- The user who owns / created the job (for permission + attribution).
    "user_uuid"    uuid NOT NULL,
    -- Free-form, handler-specific payload (target, text, recurrence metadata).
    "payload"      jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- When the job should next fire.
    "run_at"       TIMESTAMP WITH TIME ZONE NOT NULL,
    -- pending | running | done | failed | cancelled
    "status"       varchar NOT NULL DEFAULT 'pending',
    -- RFC5545-style recurrence rule (e.g. "FREQ=WEEKLY;BYDAY=MO"). NULL = one-shot.
    "recurrence"   varchar,
    "attempts"     int NOT NULL DEFAULT 0,
    "max_attempts" int NOT NULL DEFAULT 5,
    "last_error"   text,
    -- Claim bookkeeping for the worker. locked_at is cleared when the claim
    -- is released; a stale lock (locked_at older than the visibility timeout)
    -- is reclaimable so a crashed worker doesn't strand a job forever.
    "locked_at"    TIMESTAMP WITH TIME ZONE,
    "locked_by"    varchar,
    "created_at"   TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"   TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at"   TIMESTAMP WITH TIME ZONE
);

-- Hot path: the worker polls "pending jobs whose run_at <= now()". A partial
-- index keeps this scan tiny regardless of how many done/cancelled rows exist.
CREATE INDEX IF NOT EXISTS idx_scheduled_jobs_due
    ON scheduled_jobs (run_at)
    WHERE status = 'pending' AND deleted_at IS NULL;

-- For "/remind list" and per-user management screens.
CREATE INDEX IF NOT EXISTS idx_scheduled_jobs_user
    ON scheduled_jobs (user_uuid, status)
    WHERE deleted_at IS NULL;
