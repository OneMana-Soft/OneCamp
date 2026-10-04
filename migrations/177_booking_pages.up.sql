-- Migration 177: booking pages (Calendly's links, self-hosted). A person
-- publishes /book/<slug>; outsiders pick a free slot and it lands on their
-- calendar as an ordinary OneCamp event.
CREATE TABLE IF NOT EXISTS booking_pages (
    "id"                 uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "user_id"            uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "slug"               varchar NOT NULL UNIQUE,
    "title"              varchar NOT NULL,
    "description"        text NOT NULL DEFAULT '',
    "duration_minutes"   int NOT NULL CHECK (duration_minutes BETWEEN 10 AND 480),
    "hours"              jsonb NOT NULL,
    "buffer_minutes"     int NOT NULL DEFAULT 0 CHECK (buffer_minutes BETWEEN 0 AND 240),
    "min_notice_minutes" int NOT NULL DEFAULT 240 CHECK (min_notice_minutes BETWEEN 0 AND 43200),
    "max_days_ahead"     int NOT NULL DEFAULT 30 CHECK (max_days_ahead BETWEEN 1 AND 180),
    "active"             boolean NOT NULL DEFAULT true,
    "created_at"         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_booking_pages_user ON booking_pages(user_id);

-- One row per booking: the guest, the slot, the event it made, and the
-- secret that lets the guest cancel. Active rows of a page never overlap
-- (checked under a per-page lock when booking).
CREATE TABLE IF NOT EXISTS bookings (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "page_id"      uuid NOT NULL REFERENCES booking_pages(id) ON DELETE CASCADE,
    "event_uuid"   uuid,
    "guest_name"   varchar NOT NULL,
    "guest_email"  varchar NOT NULL,
    "note"         text NOT NULL DEFAULT '',
    "starts_at"    TIMESTAMP WITH TIME ZONE NOT NULL,
    "ends_at"      TIMESTAMP WITH TIME ZONE NOT NULL,
    "cancel_token" varchar NOT NULL UNIQUE,
    "cancelled_at" TIMESTAMP WITH TIME ZONE,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_bookings_page_time ON bookings(page_id, starts_at) WHERE cancelled_at IS NULL;
