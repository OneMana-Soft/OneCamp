CREATE TABLE IF NOT EXISTS teams(
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "team_name" varchar NOT NULL UNIQUE,
    "created_by" uuid REFERENCES users(id),
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);