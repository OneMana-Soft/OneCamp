CREATE TABLE IF NOT EXISTS archive_jobs (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "entity_type" varchar NOT NULL,
    "status" varchar NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'completed', 'failed', 'cancelled')),
    "started_at" TIMESTAMP WITH TIME ZONE,
    "completed_at" TIMESTAMP WITH TIME ZONE,
    "items_processed" int DEFAULT 0,
    "items_archived" int DEFAULT 0,
    "items_failed" int DEFAULT 0,
    "error_message" text,
    "metadata" jsonb,
    "triggered_by" uuid REFERENCES users(id),
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_archive_jobs_status ON archive_jobs(status);
CREATE INDEX idx_archive_jobs_entity_type ON archive_jobs(entity_type);
CREATE INDEX idx_archive_jobs_created_at ON archive_jobs(created_at);
