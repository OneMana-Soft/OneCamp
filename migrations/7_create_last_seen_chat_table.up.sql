CREATE TABLE IF NOT EXISTS last_seen_chat(
     "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
     "user_id" uuid REFERENCES users(id) NOT NULL,
     "grp_id" varchar NOT NULL,
     "user_last_seen" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT '1970-01-01 00:00:00+00',
     CONSTRAINT unique_user_grp_id UNIQUE ("user_id", "grp_id")
);