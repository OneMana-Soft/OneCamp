-- Whether an action was taken by a person, an agent, or the system itself.
--
-- The log already attributes an agent's action to the human whose credential it
-- used, which is right for accountability and silent about what actually
-- happened: the row reads as though the person did it. Enterprise buyers now
-- ask specifically for a log that distinguishes the two, and the only place the
-- distinction existed here was a free-form JSON key on one of the two agent
-- paths, which is a text search rather than a query.
--
-- Nullable with no backfill, deliberately. Existing rows keep their stored
-- hashes: the entry hash appends this field only when it is set, so history
-- verifies byte for byte as it did before, and backfilling a value into a
-- tamper-evident log would be indistinguishable from tampering with it.
ALTER TABLE admin_audit_log ADD COLUMN IF NOT EXISTS actor_kind TEXT;

-- The query this exists to make possible: everything an agent did, in order.
CREATE INDEX IF NOT EXISTS admin_audit_log_actor_kind_seq_idx
    ON admin_audit_log (actor_kind, seq DESC)
    WHERE actor_kind IS NOT NULL;
