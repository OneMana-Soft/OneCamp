-- Down 212: the cascade returns, and so does deleting a page outright. A page
-- deleted since comes back switched off, never live again by surprise, its
-- bookings kept.
UPDATE booking_pages SET active = false WHERE deleted_at IS NOT NULL;

DROP INDEX IF EXISTS idx_bookings_event;

ALTER TABLE bookings DROP CONSTRAINT IF EXISTS bookings_page_id_fkey;
ALTER TABLE bookings ADD CONSTRAINT bookings_page_id_fkey
    FOREIGN KEY (page_id) REFERENCES booking_pages(id) ON DELETE CASCADE;

ALTER TABLE bookings DROP COLUMN IF EXISTS guest_tz;
ALTER TABLE bookings DROP COLUMN IF EXISTS cancelled_by;
ALTER TABLE booking_pages DROP COLUMN IF EXISTS deleted_at;
