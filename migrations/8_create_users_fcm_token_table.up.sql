CREATE TABLE IF NOT EXISTS users_fcm_token(
     "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
     "user_id" uuid REFERENCES users(id) NOT NULL,
     "device_id" varchar NOT NULL,
     "fcm_token" varchar(255) NOT NULL,
     "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
     CONSTRAINT unique_user_device_fcm_token_id UNIQUE ("user_id", "device_id", "fcm_token")
);