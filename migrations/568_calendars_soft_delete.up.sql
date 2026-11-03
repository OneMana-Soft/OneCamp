-- V-184 head 5c: a calendar a person subscribed to (kind 'subscribed', read
-- from an address) and then unsubscribed from is kept, marked by who
-- unsubscribed and when (the owner's rule that every delete of a business
-- record is soft). calendars has carried deleted_at since 345, and every read
-- already says deleted_at IS NULL (the lookup by id reads a kept calendar as
-- gone), so only who did it is new: deleted_by, a plain uuid with no foreign
-- key, as 555's. No backfill and no index: nothing was marked before this.
ALTER TABLE calendars ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
