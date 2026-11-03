-- no-transaction
-- V-184 head 4b: a Slack channel is in one live link at a time, and a link
-- taken away (kept, marked) no longer holds it, so the channel can be linked
-- again. It replaces slack_channel_id's UNIQUE, which 560 drops.
-- CONCURRENTLY: built without blocking writes; one statement per file,
-- outside a transaction (sqlx reads the first line). A build that fails
-- leaves an INVALID index: drop it and run the migration again.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_slack_bridge_links_slack_channel_live ON slack_bridge_links (slack_channel_id) WHERE deleted_at IS NULL;
