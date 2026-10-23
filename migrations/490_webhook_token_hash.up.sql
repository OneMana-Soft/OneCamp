-- An incoming webhook's token is its credential: whoever holds it posts into
-- the workspace as the webhook. It was kept in the clear, so a copy of this
-- table let anyone post as any webhook. From here on it is found by its
-- SHA-256 (helpers/linkSecret.go) and shown once, when it is made or made
-- again; the list already hid it.
--
-- This only adds: the server from before runs on it until the new one starts,
-- reading and writing token as it did (it may be empty now). Each token
-- already given out gets its hash here, helpers.LinkSecretHash worked out by
-- sha256() over the token's text, so it keeps posting. The new server, at its
-- start, hashes whatever the old one wrote since and takes every token out of
-- the clear (models/postgres/LinkSecret). token is dropped by a later
-- migration.
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS "token_hash" bytea;
ALTER TABLE webhooks ALTER COLUMN "token" DROP NOT NULL;
UPDATE webhooks SET token_hash = sha256(convert_to(token, 'UTF8')) WHERE token_hash IS NULL AND token IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS webhooks_token_hash_key ON webhooks (token_hash);
