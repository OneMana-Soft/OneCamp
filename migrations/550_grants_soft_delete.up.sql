-- V-184 head 3b (the owner's rule: every delete is soft): stopping a
-- calendar's share, and taking someone's workspace admin role away (an admin
-- on the admin page, or the directory at sign-in), mark the row, when and by
-- whom (NULL for the directory), instead of removing it, so who was given
-- what, and who took it away and when, stays on record. Every read of the two
-- tables takes live rows only (models/postgres/SoftDelete.Live), except the
-- first-admin setup's "has the workspace had an admin", which keeps counting
-- a role taken away, so that setup stays one-time. Sharing again, or making
-- someone an admin again, brings the same row back: one row per calendar and
-- person, and per address, as before, so the keys stay.
ALTER TABLE calendar_shares ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE admin_users ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
-- First-admin setup is one-time, and a mark keeps it shut: system_configs
-- 'first_admin_setup', which domain/User CreateAdminUser writes with the
-- first admin role given. A workspace that already has an admin row, live or
-- not, or people (bots aside) has been set up, whatever admin_users holds:
-- before this migration, taking the last admin role away deleted its row, and
-- setup then let anyone who reached it make themselves the admin. Bots don't
-- count: every start makes the automation bot (and agents make theirs), so an
-- install whose owner has not finished setup has users already, and stays open.
INSERT INTO system_configs (key, value, updated_at)
SELECT 'first_admin_setup', 'done (set up before the mark)', NOW()
WHERE EXISTS (SELECT 1 FROM admin_users) OR EXISTS (SELECT 1 FROM users WHERE NOT is_bot)
ON CONFLICT (key) DO NOTHING;
