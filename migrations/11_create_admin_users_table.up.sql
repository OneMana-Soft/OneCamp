CREATE TABLE IF NOT EXISTS admin_users(
     "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
     "email_id" varchar NOT NULL UNIQUE
);