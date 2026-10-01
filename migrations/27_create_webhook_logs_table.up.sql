CREATE TABLE IF NOT EXISTS webhook_logs (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "webhook_id" uuid NOT NULL REFERENCES webhooks(id),
    "event_type" varchar NOT NULL,
    "request_body" jsonb,
    "response_status" int,
    "response_body" text,
    "error_message" text,
    "duration_ms" int,
    "success" boolean NOT NULL DEFAULT false,
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_webhook_logs_webhook_id ON webhook_logs(webhook_id);
CREATE INDEX idx_webhook_logs_created_at ON webhook_logs(created_at);
