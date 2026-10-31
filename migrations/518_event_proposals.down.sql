-- Migration 518, down: no proposals of another time. Answers and notes stay.
ALTER TABLE event_instance_answers DROP CONSTRAINT IF EXISTS event_instance_answers_proposal_whole;
ALTER TABLE event_attendees DROP CONSTRAINT IF EXISTS event_attendees_proposal_whole;

ALTER TABLE event_instance_answers DROP COLUMN IF EXISTS "proposed_against_end";
ALTER TABLE event_instance_answers DROP COLUMN IF EXISTS "proposed_against_start";
ALTER TABLE event_instance_answers DROP COLUMN IF EXISTS "proposed_end";
ALTER TABLE event_instance_answers DROP COLUMN IF EXISTS "proposed_start";

ALTER TABLE event_attendees DROP COLUMN IF EXISTS "proposed_against_end";
ALTER TABLE event_attendees DROP COLUMN IF EXISTS "proposed_against_start";
ALTER TABLE event_attendees DROP COLUMN IF EXISTS "proposed_end";
ALTER TABLE event_attendees DROP COLUMN IF EXISTS "proposed_start";
