-- gcal-7 H3: a booking page asks its own questions, and a booking keeps what
-- its guest answered, with each question's label as it was then. Both are
-- jsonb lists; neither is indexed (user text, past a btree entry's limit).
-- A constant default on PG 11+ adds each column without rewriting its table.
--
-- folded from 608_booking_team_hosts (never shipped)
-- gcal-7 H4: team booking pages. A page is solo (its owner's time alone),
-- collective (the owner and every host who accepted, booked together) or,
-- from the next head on, round robin. A host row is invited, accepted or
-- left; a host who leaves or is taken off keeps the row with left_at (every
-- delete is soft). last_booked_at and bookings.host_id are round robin's:
-- NULL host_id is the page's owner. A constant default on PG 11+ adds each
-- column without rewriting its table.
--
-- One ALTER per table (the migration lint's L6): each keeps its actions in
-- 607-then-608 order. team_mode's check is named, and dropped before it is
-- added, so the file runs twice: PG 12 adds a column's constraint even when
-- IF NOT EXISTS skips the column.
ALTER TABLE booking_pages
    ADD COLUMN IF NOT EXISTS questions jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- folded from 608_booking_team_hosts (never shipped)
    ADD COLUMN IF NOT EXISTS team_mode text NOT NULL DEFAULT 'solo',
    DROP CONSTRAINT IF EXISTS booking_pages_team_mode_check,
    ADD CONSTRAINT booking_pages_team_mode_check
        CHECK (team_mode IN ('solo', 'collective', 'round_robin'));

-- folded from 608_booking_team_hosts (never shipped)
CREATE TABLE IF NOT EXISTS booking_page_hosts (
    page_id        uuid        NOT NULL REFERENCES booking_pages(id),
    user_id        uuid        NOT NULL,
    invited_at     timestamptz NOT NULL DEFAULT now(),
    accepted_at    timestamptz NULL,
    left_at        timestamptz NULL,
    last_booked_at timestamptz NULL,
    PRIMARY KEY (page_id, user_id)
);

-- "Pages I host": a member's own rows.
CREATE INDEX IF NOT EXISTS idx_booking_page_hosts_user ON booking_page_hosts (user_id);

ALTER TABLE bookings
    ADD COLUMN IF NOT EXISTS answers jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- folded from 608_booking_team_hosts (never shipped)
    ADD COLUMN IF NOT EXISTS host_id uuid NULL;
