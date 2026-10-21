DROP INDEX IF EXISTS idx_admin_audit_log_doc_events;
ALTER TABLE users_notification_preferences DROP COLUMN IF EXISTS record_views;
DROP INDEX IF EXISTS resource_view_days_day;
DROP TABLE IF EXISTS resource_view_days;
