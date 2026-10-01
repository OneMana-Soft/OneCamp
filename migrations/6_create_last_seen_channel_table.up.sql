CREATE TABLE IF NOT EXISTS last_seen_channel(
     "user_id" uuid REFERENCES users(id) NOT NULL,
     "channel_id" uuid REFERENCES channels(id) NOT NULL,
     "user_last_seen" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT '1970-01-01 00:00:00+00',
     PRIMARY KEY ("user_id", "channel_id")
);