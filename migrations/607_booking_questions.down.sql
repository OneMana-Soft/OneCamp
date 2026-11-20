-- The exact reverse of the up: 608_booking_team_hosts's undo first (folded
-- in here, never shipped), then 607's own, one ALTER per table.
ALTER TABLE bookings
    DROP COLUMN IF EXISTS host_id,
    DROP COLUMN IF EXISTS answers;
DROP INDEX IF EXISTS idx_booking_page_hosts_user;
DROP TABLE IF EXISTS booking_page_hosts;
ALTER TABLE booking_pages
    DROP CONSTRAINT IF EXISTS booking_pages_team_mode_check,
    DROP COLUMN IF EXISTS team_mode,
    DROP COLUMN IF EXISTS questions;
