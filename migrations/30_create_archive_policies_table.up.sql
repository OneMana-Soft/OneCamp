CREATE TABLE IF NOT EXISTS archive_policies (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "entity_type" varchar NOT NULL UNIQUE,
    "retention_days" int NOT NULL DEFAULT 365,
    "auto_archive" boolean NOT NULL DEFAULT false,
    "archive_completed_tasks" boolean DEFAULT true,
    "archive_inactive_channels_days" int DEFAULT 90,
    "compress_attachments" boolean DEFAULT false,
    "created_by" uuid REFERENCES users(id),
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

INSERT INTO archive_policies (entity_type, retention_days, auto_archive) VALUES
    ('posts', 365, false),
    ('chats', 365, false),
    ('tasks', 180, false),
    ('recordings', 90, false),
    ('attachments', 365, false),
    ('docs', 365, false)
ON CONFLICT (entity_type) DO NOTHING;
