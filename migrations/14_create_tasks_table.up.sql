CREATE TABLE IF NOT EXISTS tasks(
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "project_id" uuid REFERENCES projects(id),
    "created_by" uuid REFERENCES users(id),
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);