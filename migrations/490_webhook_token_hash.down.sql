-- A token kept only as its hash can't be put back in the clear. Each webhook
-- gets a stand-in no sender holds ("void-" and its hash), so token is whole
-- and unique again, and its token is made again with the code from before.
UPDATE webhooks SET token = 'void-' || encode(token_hash, 'hex') WHERE token IS NULL;
ALTER TABLE webhooks ALTER COLUMN "token" SET NOT NULL;
DROP INDEX IF EXISTS webhooks_token_hash_key;
ALTER TABLE webhooks DROP COLUMN IF EXISTS "token_hash";
