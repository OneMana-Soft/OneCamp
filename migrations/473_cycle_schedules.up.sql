-- B3b, the Cycle Scheduler (design/projects-vs-asana-jira/PROGRAM.md D-PJ-6):
-- a project whose cycles run themselves has one row. No row, cycles by hand,
-- as before. The number is the next free at landing (the integrator renumbers).
CREATE TABLE IF NOT EXISTS cycle_schedules (
    "project_uuid"   uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    "weeks"          int NOT NULL CHECK (weeks BETWEEN 1 AND 8),
    "cooldown_weeks" int NOT NULL DEFAULT 0 CHECK (cooldown_weeks BETWEEN 0 AND 1),
    "upcoming"       int NOT NULL DEFAULT 2 CHECK (upcoming BETWEEN 1 AND 15),
    "roll_over"      boolean NOT NULL DEFAULT true,
    "anchor"         TIMESTAMP WITH TIME ZONE NOT NULL,
    "set_by"         uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
-- Every foreign key has an index leading with it (deleting a user looks here).
CREATE INDEX IF NOT EXISTS idx_fk_cycle_schedules_set_by ON cycle_schedules (set_by) WHERE set_by IS NOT NULL;
