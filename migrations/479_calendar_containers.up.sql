-- Calendar A6: a calendar that belongs to something, a project first (then
-- a channel or a team): its members see it as the container decides. One
-- per container, found again by it.

ALTER TABLE calendars ADD COLUMN IF NOT EXISTS "container_kind" varchar(16) NOT NULL DEFAULT ''
    CHECK (container_kind IN ('', 'project', 'channel', 'team'));
ALTER TABLE calendars ADD COLUMN IF NOT EXISTS "container_id" text NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS calendars_one_per_container_idx
    ON calendars (container_kind, container_id) WHERE container_kind <> '' AND deleted_at IS NULL;

-- What is on a calendar, by time: a project's calendar is read by the
-- calendar, not by who made each event, and a calendar deleted finds its
-- events by it too.
CREATE INDEX IF NOT EXISTS events_by_calendar_idx ON events (calendar_id, start_at);
-- It leads with calendar_id, so it serves the foreign key as well: the
-- foreign-key sweep's single-column index would be a second copy for every
-- write to events to keep up.
DROP INDEX IF EXISTS idx_fk_events_calendar_id;

-- A person's calendar shared with someone at a level (A6, SC-25): 'details'
-- sees what is on it, 'edit' changes it, 'manage' shares it too. Without a
-- share, a member sees only when the person is busy (owner decision O7).
CREATE TABLE IF NOT EXISTS calendar_shares (
    "calendar_id" uuid NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    "person_id"   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "level"       varchar(12) NOT NULL CHECK (level IN ('details', 'edit', 'manage')),
    "granted_by"  uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"  timestamptz NOT NULL DEFAULT NOW(),
    "updated_at"  timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (calendar_id, person_id)
);

CREATE INDEX IF NOT EXISTS calendar_shares_person_idx ON calendar_shares (person_id);
-- Who shared it: deleting a person clears it here.
CREATE INDEX IF NOT EXISTS calendar_shares_granted_by_idx ON calendar_shares (granted_by) WHERE granted_by IS NOT NULL;
