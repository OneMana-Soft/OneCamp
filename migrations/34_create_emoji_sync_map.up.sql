CREATE TABLE IF NOT EXISTS emoji_sync_map (
    "github_name" text PRIMARY KEY,
    "onecamp_uuid" uuid NOT NULL
);
