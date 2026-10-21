-- no-transaction
-- A channel's posts by time: every channel list and user load counts unread
-- posts by post_channel and created_at (domain/Post
-- GetLatestPostInChannelCountByUserID, domain/LastSeenChannel), and posts had
-- no index on post_channel at all. Not partial, so it also serves the
-- posts.post_channel foreign key when a channel is deleted.
-- CONCURRENTLY: built without blocking writes; one statement per file, outside
-- a transaction (sqlx reads the first line). A build that fails leaves an
-- INVALID index: drop it and run the migration again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_posts_channel_created ON posts (post_channel, created_at);
