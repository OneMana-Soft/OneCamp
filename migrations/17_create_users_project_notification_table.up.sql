CREATE TABLE IF NOT EXISTS users_project_notification(
     "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
     "user_id" uuid REFERENCES users(id) NOT NULL,
     "project_id" uuid REFERENCES projects(id) NOT NULL,
     "notification_type" notification_type_enum NOT NULL,
     "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
     CONSTRAINT unique_user_project_id UNIQUE ("user_id", "project_id")
);