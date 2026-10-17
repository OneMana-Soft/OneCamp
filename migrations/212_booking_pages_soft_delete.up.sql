-- Migration 212: a booking outlives its page (CAL-BUG-3), and says who
-- cancelled it.
--
-- Deleting a booking page deleted every booking made through it (177's ON
-- DELETE CASCADE): each guest's name, email, note and cancel link. The guest's
-- "change or cancel" link answered "We couldn't find that booking" while the
-- meeting stayed on the owner's calendar. A page is now deleted by marking it
-- (deleted_at): it stops taking bookings and leaves its owner's list, its
-- address stays taken (a new page can't take an old guest's link), and its
-- bookings stay, their links working.
--
-- The cascade goes: a page with bookings can't be removed outright
-- (RESTRICT). The application never removes a user outright (it marks them
-- gone), so the cascade from users to pages only ever meets pages without
-- bookings; an operator deleting a user row by hand deletes their bookings
-- first.
--
-- cancelled_by is who cancelled: 'guest' through their link, or 'host' by
-- deleting the meeting (CAL-BUG-4), so the guest's page can say "Sam cancelled
-- this meeting". guest_tz is the zone the guest booked in, so a later email
-- (the meeting moved or cancelled) gives the time as they read it. event_uuid
-- is indexed: every change to an event looks for a booking it made.
ALTER TABLE booking_pages ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE;

ALTER TABLE bookings ADD COLUMN IF NOT EXISTS "cancelled_by" varchar(8)
    CHECK (cancelled_by IN ('guest', 'host'));
ALTER TABLE bookings ADD COLUMN IF NOT EXISTS "guest_tz" varchar(64) NOT NULL DEFAULT '';

ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_page_id_fkey;
ALTER TABLE bookings ADD CONSTRAINT bookings_page_id_fkey
    FOREIGN KEY (page_id) REFERENCES booking_pages(id) ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS idx_bookings_event ON bookings(event_uuid) WHERE event_uuid IS NOT NULL;
