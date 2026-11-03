-- V-184 head 4b: three things an admin takes away from the workspace's setup
-- are kept, marked by who took them away and when (the owner's rule that every
-- delete of a business record is soft): a custom AI provider, a model on the
-- AI allowlist, and a link between a Slack channel and a OneCamp channel.
-- deleted_by is a plain uuid, no foreign key, as 957's. No backfill and no
-- index here: every read says deleted_at IS NULL, and 556-559 make each
-- table's unique names hold among live rows only, so a provider, a model or a
-- link added again is a new row beside the one taken away.
ALTER TABLE ai_providers ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE ai_authorized_models ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE slack_bridge_links ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
