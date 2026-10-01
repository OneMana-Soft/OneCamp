-- Down migration 101: drop the durable AI pending-actions table.
DROP TABLE IF EXISTS ai_pending_actions;
