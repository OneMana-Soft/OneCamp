-- Rooms (gcal-6, Resource Directory #47): the meeting rooms admins keep for
-- the workspace (the install is the workspace, as calendars and events are),
-- and which events hold them. A room is retired, never removed (deleted_at):
-- it keeps its past bookings and takes no new one. A booking ties a room to an
-- event and the span the event covers (its first start to its last end; NULL
-- for a series without an end); a booking given up is kept (deleted_at).
CREATE TABLE IF NOT EXISTS calendar_rooms (
    "id"         uuid PRIMARY KEY,
    "name"       text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    "building"   text NOT NULL DEFAULT '' CHECK (char_length(building) <= 120),
    "floor"      text NOT NULL DEFAULT '' CHECK (char_length(floor) <= 40),
    "capacity"   integer NOT NULL DEFAULT 0 CHECK (capacity BETWEEN 0 AND 10000),
    "features"   text[] NOT NULL DEFAULT '{}',
    "created_by" uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at" timestamptz NOT NULL DEFAULT now(),
    "updated_by" uuid REFERENCES users(id) ON DELETE SET NULL,
    "updated_at" timestamptz NOT NULL DEFAULT now(),
    "deleted_at" timestamptz
);

-- One live room by a name (names are short: under the btree entry limit).
CREATE UNIQUE INDEX IF NOT EXISTS idx_calendar_rooms_live_name
    ON calendar_rooms (lower(name)) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_fk_calendar_rooms_created_by ON calendar_rooms (created_by);
CREATE INDEX IF NOT EXISTS idx_fk_calendar_rooms_updated_by ON calendar_rooms (updated_by);

CREATE TABLE IF NOT EXISTS calendar_room_bookings (
    "id"         uuid PRIMARY KEY,
    "room_id"    uuid NOT NULL REFERENCES calendar_rooms(id) ON DELETE CASCADE,
    "event_id"   uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    "starts_at"  timestamptz NOT NULL,
    "ends_at"    timestamptz,
    "created_by" uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at" timestamptz NOT NULL DEFAULT now(),
    "deleted_by" uuid REFERENCES users(id) ON DELETE SET NULL,
    "deleted_at" timestamptz
);

-- An event holds a room once.
CREATE UNIQUE INDEX IF NOT EXISTS idx_calendar_room_bookings_live
    ON calendar_room_bookings (room_id, event_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_fk_calendar_room_bookings_room ON calendar_room_bookings (room_id, starts_at);
CREATE INDEX IF NOT EXISTS idx_fk_calendar_room_bookings_event ON calendar_room_bookings (event_id);
CREATE INDEX IF NOT EXISTS idx_fk_calendar_room_bookings_created_by ON calendar_room_bookings (created_by);
CREATE INDEX IF NOT EXISTS idx_fk_calendar_room_bookings_deleted_by ON calendar_room_bookings (deleted_by);
