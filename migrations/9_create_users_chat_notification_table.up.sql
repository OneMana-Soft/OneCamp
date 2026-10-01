CREATE TYPE notification_type_enum AS ENUM ('all', 'mention', 'block');

CREATE TABLE IF NOT EXISTS users_chat_notification(
     "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
     "user_id" uuid REFERENCES users(id) NOT NULL,
     "grp_id" varchar(255) NOT NULL,
     "notification_type" notification_type_enum NOT NULL,
     "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
     CONSTRAINT unique_user_and_to_id_and_notification_type UNIQUE ("user_id", "grp_id")
);