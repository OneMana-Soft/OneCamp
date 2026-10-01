CREATE TABLE IF NOT EXISTS invitations(
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "email" text NOT NULL,
    "invited_by" uuid REFERENCES users(id),
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);