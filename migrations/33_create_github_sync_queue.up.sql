CREATE TABLE IF NOT EXISTS github_sync_queue (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "task_id" uuid NOT NULL REFERENCES tasks(id),
    "sync_type" varchar NOT NULL CHECK (sync_type IN ('status', 'name', 'description', 'assignee', 'label', 'comment', 'reaction')),
    "payload" jsonb NOT NULL,
    "status" varchar NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    "attempts" int NOT NULL DEFAULT 0,
    "next_retry_at" TIMESTAMP WITH TIME ZONE,
    "error_message" text,
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sync_queue_status ON github_sync_queue(status, next_retry_at);
