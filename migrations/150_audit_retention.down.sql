DROP INDEX IF EXISTS idx_admin_audit_log_retention;
ALTER TABLE admin_audit_log DROP COLUMN IF EXISTS "redacted_at";
