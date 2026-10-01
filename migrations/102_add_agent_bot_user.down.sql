-- Down migration 102: drop the per-agent bot principal link.
-- The provisioned bot users/nodes are left in place (they own authored
-- messages); only the denormalized link column is removed.
ALTER TABLE ai_agents DROP COLUMN IF EXISTS bot_user_id;
