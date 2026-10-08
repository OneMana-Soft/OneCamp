-- Read receipts in DMs and group chats.
--
-- "Who in this conversation has seen it, up to when" reads last_seen_chat by
-- grp_id; its only index starts with user_id.
CREATE INDEX IF NOT EXISTS idx_last_seen_chat_grp_id ON last_seen_chat (grp_id);

-- Whether a person lets others see when they've read messages. Off, they
-- don't see others' either (Teams' rule). On by default, as in Teams and Zulip.
ALTER TABLE users_notification_preferences ADD COLUMN IF NOT EXISTS read_receipts BOOLEAN NOT NULL DEFAULT TRUE;
