-- Revert migration 77.

-- Workflows using a new trigger_type would violate the restored CHECK, so
-- neutralise them back to the original 'message_posted' first.
UPDATE workflows SET trigger_type = 'message_posted'
    WHERE trigger_type NOT IN ('message_posted');

ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_trigger_type_check;
ALTER TABLE workflows ADD CONSTRAINT workflows_trigger_type_check
    CHECK (trigger_type IN ('message_posted'));

ALTER TABLE workflows DROP COLUMN IF EXISTS trigger_config;
ALTER TABLE workflows DROP COLUMN IF EXISTS bot_name;

-- Bot identity. Soft-delete the seeded system bot so its posts keep resolving
-- (FK references remain valid) while it disappears from active use, then drop
-- the column.
UPDATE users SET deleted_at = NOW() WHERE email_id = 'automation@bot.onecamp.local';
ALTER TABLE users DROP COLUMN IF EXISTS is_bot;
