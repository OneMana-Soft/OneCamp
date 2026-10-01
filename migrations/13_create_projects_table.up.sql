CREATE TABLE IF NOT EXISTS projects(
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "project_name" varchar NOT NULL UNIQUE,
    "team_id" uuid REFERENCES teams(id),
    "created_by" uuid REFERENCES users(id),
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE,
    CONSTRAINT unique_project_name_and_team_id UNIQUE ("project_name", "team_id")
);