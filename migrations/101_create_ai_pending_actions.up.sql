-- Migration 101: durable AI pending actions (in-thread write approvals).
--
-- When the AI assistant / a bot / an agent proposes a WRITE action, it is
-- persisted here as a durable, approvable record instead of an ephemeral
-- client-side dialog. The action survives tab close / navigation / reload and
-- is surfaced as an in-thread Approve/Deny card. On approval it executes
-- server-side, AT MOST ONCE, AS the approver, with that user's permissions
-- re-checked at execution time (no confused-deputy: the bot holds no standalone
-- privilege).
--
-- Status lifecycle:
--   pending -> executing -> executed | failed   (approve)
--   pending -> rejected                          (deny)
--   pending -> expired                           (TTL passed, never executed)
-- The pending -> executing transition is a guarded UPDATE so two approvals
-- (double click / two tabs / two devices) execute the action only once.

CREATE TABLE IF NOT EXISTS ai_pending_actions (
    "id"              uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    -- The user the action is for AND who may approve it (it executes as them,
    -- with their permissions). v1: requester == approver.
    "requested_by"    uuid NOT NULL REFERENCES users(id),
    -- Where the proposal was surfaced, so the FE can render the card in place
    -- and reconcile on load. 'channel' | 'dm' | 'group' | 'assistant'.
    "surface_type"    varchar NOT NULL,
    "surface_id"      varchar NOT NULL DEFAULT '',
    -- The tool + params to run (validated again at approval time).
    "tool_name"       varchar NOT NULL,
    "params"          jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- Human-readable summary shown on the card.
    "description"     text NOT NULL DEFAULT '',
    -- pending | executing | executed | failed | rejected | expired
    "status"          varchar NOT NULL DEFAULT 'pending',
    -- Dedupe key so the same proposed action isn't persisted twice (e.g. a
    -- retried generation). Unique when present.
    "idempotency_key" varchar,
    -- Result/error captured after execution (for the card + audit).
    "result"          text,
    "error"           text,
    "expires_at"      timestamptz NOT NULL,
    "created_at"      timestamptz NOT NULL DEFAULT now(),
    "resolved_at"     timestamptz,
    "resolved_by"     uuid REFERENCES users(id)
);

-- Reconcile-on-load: a user's open (pending, not expired) actions, newest first.
CREATE INDEX IF NOT EXISTS idx_ai_pending_actions_open
    ON ai_pending_actions (requested_by, created_at DESC)
    WHERE status = 'pending';

-- Dedupe guard.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_pending_actions_idem
    ON ai_pending_actions (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
