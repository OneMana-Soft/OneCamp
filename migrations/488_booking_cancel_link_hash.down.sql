-- A link kept only as its hash and its seal can't be put back in the clear:
-- the hash is one-way, and the key that opens the seal isn't in the database.
-- Each booking gets a stand-in that no link carries ("void-" and its hash), so
-- cancel_token is whole and unique again, and its guest's link stops working
-- with the code from before this.
UPDATE bookings SET cancel_token = 'void-' || encode(cancel_token_hash, 'hex') WHERE cancel_token IS NULL;
ALTER TABLE bookings ALTER COLUMN "cancel_token" SET NOT NULL;
DROP INDEX IF EXISTS bookings_cancel_token_hash_key;
ALTER TABLE bookings DROP COLUMN IF EXISTS "cancel_token_sealed";
ALTER TABLE bookings DROP COLUMN IF EXISTS "cancel_token_hash";
