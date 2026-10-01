-- Migration 106: make the admin audit log tamper-evident (hash chain).
--
-- Each new entry stores a hash over its own content plus the previous entry's
-- hash, so any later insertion, edit, or deletion of historical rows breaks
-- verification. Additive + backward-compatible: existing rows keep NULL hashes
-- and verification simply starts at the first hashed row (no false alarm for
-- pre-feature history). `seq` gives a deterministic chain order independent of
-- timestamp ties.

ALTER TABLE admin_audit_log ADD COLUMN IF NOT EXISTS seq BIGSERIAL;
ALTER TABLE admin_audit_log ADD COLUMN IF NOT EXISTS entry_hash varchar;
ALTER TABLE admin_audit_log ADD COLUMN IF NOT EXISTS prev_hash varchar;

-- Chain walk / latest-in-chain lookups order by seq.
CREATE INDEX IF NOT EXISTS idx_admin_audit_log_seq ON admin_audit_log (seq);
