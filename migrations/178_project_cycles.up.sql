-- Migration 178: cycles (Linear's sprints). A project works in numbered,
-- time-boxed cycles that never overlap. A task is in at most one cycle;
-- completing a cycle can carry its unfinished tasks into the next one, and
-- the cycle keeps how it ended (done and carried-over counts).
CREATE TABLE IF NOT EXISTS project_cycles (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "project_uuid" uuid NOT NULL,
    "number"       int NOT NULL,
    "name"         varchar NOT NULL DEFAULT '',
    "starts_at"    TIMESTAMP WITH TIME ZONE NOT NULL,
    "ends_at"      TIMESTAMP WITH TIME ZONE NOT NULL CHECK (ends_at > starts_at),
    "completed_at" TIMESTAMP WITH TIME ZONE,
    "done_count"   int,
    "carried_count" int,
    "created_by"   uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE (project_uuid, number)
);
CREATE INDEX IF NOT EXISTS idx_project_cycles_project ON project_cycles(project_uuid, starts_at);

CREATE TABLE IF NOT EXISTS task_cycles (
    "task_uuid" uuid PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    "cycle_id"  uuid NOT NULL REFERENCES project_cycles(id) ON DELETE CASCADE,
    "added_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_task_cycles_cycle ON task_cycles(cycle_id);
