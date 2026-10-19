-- Migration 362: reminders for the calendar's events (calendar programme A5,
-- working number 264). It refers to 345's events, so it sorts after them; the
-- integrator gives it the next free number above beta's top when it merges.
--
-- Each person's reminders for an event or a series, and the one queue every
-- delayed piece of work already uses (scheduled_jobs, migration 72) for when
-- each next fires: one waiting job per person and event, which fires, then
-- names the next.
--
-- Postgres 12: ids are made in Go, and every statement can run twice.

-- A person's own reminders for an event or a series: how many minutes before
-- it each fires (0 at the start; for an all-day event, before 00:00 of its
-- first day in the person's zone, so 900 is 09:00 the day before). No row is
-- the default (10 minutes before, or 09:00 the day before an all-day event,
-- for what they organise or said yes or maybe to); an empty list is none.
CREATE TABLE IF NOT EXISTS event_reminders (
    "event_id"   uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    "user_id"    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "minutes"    integer[] NOT NULL DEFAULT '{}',
    "updated_at" timestamptz NOT NULL,
    PRIMARY KEY (event_id, user_id),
    CONSTRAINT event_reminders_count_check CHECK (coalesce(array_length(minutes, 1), 0) <= 5)
);

-- One waiting reminder job per person, event and kind (the next one, or a
-- snooze), found again when the event, the person's answer or their
-- reminders change.
CREATE UNIQUE INDEX IF NOT EXISTS scheduled_jobs_calendar_reminder_idx
    ON scheduled_jobs (user_uuid, (payload->>'event_id'), (payload->>'kind'))
    WHERE job_type = 'calendar_reminder' AND status = 'pending' AND deleted_at IS NULL;

-- An event's reminder jobs, whoever's: what a change to the event moves or
-- cancels, and what the planner looks for to find events nobody planned.
CREATE INDEX IF NOT EXISTS scheduled_jobs_calendar_reminder_event_idx
    ON scheduled_jobs ((payload->>'event_id'))
    WHERE job_type = 'calendar_reminder' AND status IN ('pending', 'running') AND deleted_at IS NULL;
