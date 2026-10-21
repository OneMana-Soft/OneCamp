DROP INDEX IF EXISTS idx_threads_last_reply;
DROP INDEX IF EXISTS idx_thread_participants_user;
ALTER TABLE threads DROP COLUMN IF EXISTS last_reply_at;
ALTER TABLE thread_participants DROP COLUMN IF EXISTS follow;
