-- An invitation's link is the invited person's credential: whoever holds it
-- joins the workspace as them. It was kept in the clear, so a copy of this
-- table let anyone sign up as someone invited who hasn't joined yet. From
-- here on it is found by its SHA-256 (helpers/linkSecret.go); a link made from
-- now on is also kept sealed with the server's key, so the admin's list can
-- copy it.
--
-- This only adds: the server from before runs on it until the new one starts,
-- reading and writing token as it did. Each link already sent gets its hash
-- here, helpers.LinkSecretHash worked out by sha256() over the link's text,
-- so it keeps working; it can't be sealed, so the list offers Send again for
-- it instead of a link to copy. The new server, at its start, hashes whatever
-- the old one wrote since and takes every link out of the clear
-- (models/postgres/LinkSecret). token is dropped by a later migration.
ALTER TABLE invitations ADD COLUMN IF NOT EXISTS "token_hash" bytea;
ALTER TABLE invitations ADD COLUMN IF NOT EXISTS "token_sealed" text NOT NULL DEFAULT '';
UPDATE invitations SET token_hash = sha256(convert_to(token, 'UTF8')) WHERE token_hash IS NULL AND token IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS invitations_token_hash_key ON invitations (token_hash) WHERE token_hash IS NOT NULL;
