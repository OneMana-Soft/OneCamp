-- Migration 518: a guest proposes another time for an
-- event, or for one instance of a repeating event (calendar head 4, gcal-4).
--
-- A proposal is the time proposed, and the time the event was at when it was
-- proposed: it stands only while the event is still at that time, so any
-- change of the event's time sets it aside without a write. It rides the
-- guest's answer: on event_attendees for a single event, and on
-- event_instance_answers for one instance of a series.
--
-- Postgres 12: every statement can run twice.

ALTER TABLE event_attendees ADD COLUMN IF NOT EXISTS "proposed_start" timestamptz;
ALTER TABLE event_attendees ADD COLUMN IF NOT EXISTS "proposed_end" timestamptz;
ALTER TABLE event_attendees ADD COLUMN IF NOT EXISTS "proposed_against_start" timestamptz;
ALTER TABLE event_attendees ADD COLUMN IF NOT EXISTS "proposed_against_end" timestamptz;

ALTER TABLE event_instance_answers ADD COLUMN IF NOT EXISTS "proposed_start" timestamptz;
ALTER TABLE event_instance_answers ADD COLUMN IF NOT EXISTS "proposed_end" timestamptz;
ALTER TABLE event_instance_answers ADD COLUMN IF NOT EXISTS "proposed_against_start" timestamptz;
ALTER TABLE event_instance_answers ADD COLUMN IF NOT EXISTS "proposed_against_end" timestamptz;

-- A proposal is whole or absent, and ends after it starts: each part named,
-- as a CHECK that comes to NULL passes. Added NOT VALID, then validated. The
-- migration runs in one transaction, so ADD CONSTRAINT's lock on the table
-- is held through VALIDATE's scan of its rows; the scan is short, as no row
-- has a proposal yet.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'event_attendees_proposal_whole') THEN
        ALTER TABLE event_attendees ADD CONSTRAINT event_attendees_proposal_whole CHECK (
            (proposed_start IS NULL AND proposed_end IS NULL AND proposed_against_start IS NULL AND proposed_against_end IS NULL)
            OR (proposed_start IS NOT NULL AND proposed_end IS NOT NULL AND proposed_end > proposed_start
                AND proposed_against_start IS NOT NULL AND proposed_against_end IS NOT NULL)) NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'event_instance_answers_proposal_whole') THEN
        ALTER TABLE event_instance_answers ADD CONSTRAINT event_instance_answers_proposal_whole CHECK (
            (proposed_start IS NULL AND proposed_end IS NULL AND proposed_against_start IS NULL AND proposed_against_end IS NULL)
            OR (proposed_start IS NOT NULL AND proposed_end IS NOT NULL AND proposed_end > proposed_start
                AND proposed_against_start IS NOT NULL AND proposed_against_end IS NOT NULL)) NOT VALID;
    END IF;
END $$;
ALTER TABLE event_attendees VALIDATE CONSTRAINT event_attendees_proposal_whole;
ALTER TABLE event_instance_answers VALIDATE CONSTRAINT event_instance_answers_proposal_whole;
