-- Migration 589: a new agent asks first (ADMIN-16).
--
-- An agent made without choosing how far it may go on its own used to act on
-- its own ('auto'). Now it proposes each change for a yes in the Inbox
-- ('approval'), until someone chooses otherwise. Only the column's default
-- moves: every existing agent keeps the mode it has.
ALTER TABLE ai_agents ALTER COLUMN "autonomy" SET DEFAULT 'approval';
