-- A booking's cancel link is its guest's credential: whoever holds it cancels
-- the booking. It was kept in the clear, so a copy of this table could cancel
-- anyone's booking. From here on it is found by its SHA-256
-- (helpers/linkSecret.go); a link made from now on is also kept sealed with
-- the server's key, so a moved booking's email can send it again.
--
-- This only adds: the server from before runs on it until the new one starts,
-- reading and writing cancel_token as it did (it may be empty now). Each link
-- already mailed gets its hash here, helpers.LinkSecretHash worked out by
-- sha256() over the link's text, so it keeps cancelling. The new server, at
-- its start, hashes whatever the old one wrote since and takes every link out
-- of the clear (models/postgres/LinkSecret). cancel_token is dropped by a
-- later migration.
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS "cancel_token_hash" bytea;
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS "cancel_token_sealed" text NOT NULL DEFAULT '';
ALTER TABLE bookings ALTER COLUMN "cancel_token" DROP NOT NULL;
UPDATE bookings SET cancel_token_hash = sha256(convert_to(cancel_token, 'UTF8'))
 WHERE cancel_token_hash IS NULL AND cancel_token IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS bookings_cancel_token_hash_key ON bookings (cancel_token_hash);
