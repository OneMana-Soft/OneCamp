CREATE TABLE IF NOT EXISTS last_seen_activity(
     "user_id" uuid PRIMARY KEY REFERENCES users(id) NOT NULL,
     "user_last_seen" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
