-- Migration 928 (a placeholder; the integrator numbers it): a project whose
-- cycles run themselves can have a task that starts (moves from Backlog or
-- To do into a started status) join the cycle running then, as Linear's
-- "auto-add started issues" does (linear-12; business/Cycle/startedJoins.go).
ALTER TABLE cycle_schedules ADD COLUMN IF NOT EXISTS add_started boolean NOT NULL DEFAULT false;
