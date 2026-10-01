CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS channels (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "ch_name" varchar NOT NULL UNIQUE,
    "ch_private" boolean NOT NULL,
    "created_by" uuid REFERENCES users(id),
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);
