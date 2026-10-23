-- A link kept only as its hash and its seal can't be put back in the clear:
-- the hash is one-way, and the key that opens the seal isn't in the
-- database. Those invitations are left with no link, and are sent again.
DROP INDEX IF EXISTS invitations_token_hash_key;
ALTER TABLE invitations DROP COLUMN IF EXISTS "token_sealed";
ALTER TABLE invitations DROP COLUMN IF EXISTS "token_hash";
