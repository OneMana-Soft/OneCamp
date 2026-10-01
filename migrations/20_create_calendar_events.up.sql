CREATE TABLE IF NOT EXISTS calendar_events(
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "title" varchar NOT NULL,
    "description" text,
    "start_time" TIMESTAMP WITH TIME ZONE NOT NULL,
    "end_time" TIMESTAMP WITH TIME ZONE NOT NULL,
    "created_by" uuid REFERENCES users(id) ON DELETE CASCADE,
    "google_calendar_event_id" text,
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);