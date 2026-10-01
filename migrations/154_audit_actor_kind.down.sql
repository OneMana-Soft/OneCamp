DROP INDEX IF EXISTS admin_audit_log_actor_kind_seq_idx;
ALTER TABLE admin_audit_log DROP COLUMN IF EXISTS actor_kind;
