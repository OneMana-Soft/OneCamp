-- Migration 106 down: drop the hash-chain columns.
DROP INDEX IF EXISTS idx_admin_audit_log_seq;
ALTER TABLE admin_audit_log DROP COLUMN IF EXISTS prev_hash;
ALTER TABLE admin_audit_log DROP COLUMN IF EXISTS entry_hash;
ALTER TABLE admin_audit_log DROP COLUMN IF EXISTS seq;
