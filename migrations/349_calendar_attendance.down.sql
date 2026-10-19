-- Migration 349, down: no answers for one instance alone, and no answer links.
-- Guests invited by email keep their rows, which 345 already allows, with
-- their answers; their links stop working.
ALTER TABLE events DROP COLUMN IF EXISTS "email_on_answers";

DROP INDEX IF EXISTS event_attendees_rsvp_token_idx;
ALTER TABLE event_attendees DROP COLUMN IF EXISTS "nudged_at";
ALTER TABLE event_attendees DROP COLUMN IF EXISTS "rsvp_token_sealed";
ALTER TABLE event_attendees DROP COLUMN IF EXISTS "rsvp_token_hash";
ALTER TABLE event_attendees DROP COLUMN IF EXISTS "invite_state";
ALTER TABLE event_attendees DROP COLUMN IF EXISTS "name";

DROP TABLE IF EXISTS event_instance_answers;
