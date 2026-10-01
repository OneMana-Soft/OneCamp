-- Migration 150: retention for the audit log, by redaction rather than deletion.
--
-- THE PROBLEM. Nothing ever deleted an audit entry. That satisfies the AI Act's
-- six-month minimum by never expiring, and it is the wrong default for a
-- deployer who has to delete on a schedule: a log that grows forever is a
-- data-minimisation problem and, eventually, a disk problem.
--
-- WHY NOT DELETE. Each entry hashes its own contents plus the previous entry's
-- hash. Removing a row breaks every verification after it, so a retention job
-- that deletes would trade tamper-evidence for tidiness, which is the wrong way
-- round: the chain is the reason the log is worth keeping at all.
--
-- SO: REDACT. The row and its hashes stay, the content columns are cleared, and
-- redacted_at records that it happened. The chain's continuity survives because
-- no link is removed. What does not survive is the ability to recompute a
-- redacted row's hash from its content, which is the honest cost and is why
-- Verify reports redactions separately instead of quietly counting them as
-- checked.
--
-- This is the shape the literature calls a dangling pointer or crypto-erasure:
-- content deletion satisfies the erasure requirement, the persisting hash record
-- satisfies the integrity one.
ALTER TABLE admin_audit_log
    ADD COLUMN IF NOT EXISTS "redacted_at" TIMESTAMP WITH TIME ZONE;

-- The retention sweep looks for old, not-yet-redacted rows.
CREATE INDEX IF NOT EXISTS idx_admin_audit_log_retention
    ON admin_audit_log (created_at)
    WHERE redacted_at IS NULL;
