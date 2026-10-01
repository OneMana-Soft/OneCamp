ALTER TABLE invitations DROP COLUMN IF EXISTS "status";
ALTER TABLE invitations DROP COLUMN IF EXISTS "token";
ALTER TABLE invitations DROP COLUMN IF EXISTS "token_expires_at";
