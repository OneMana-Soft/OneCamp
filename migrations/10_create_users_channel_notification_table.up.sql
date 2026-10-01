CREATE TABLE IF NOT EXISTS users_channel_notification(
     "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
     "user_id" uuid REFERENCES users(id) NOT NULL,
     "channel_id" uuid REFERENCES channels(id) NOT NULL,
     "notification_type" notification_type_enum NOT NULL,
     "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
     CONSTRAINT unique_user_channel_id UNIQUE ("user_id", "channel_id")
);