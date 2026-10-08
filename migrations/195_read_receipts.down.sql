ALTER TABLE users_notification_preferences DROP COLUMN IF EXISTS read_receipts;
DROP INDEX IF EXISTS idx_last_seen_chat_grp_id;
