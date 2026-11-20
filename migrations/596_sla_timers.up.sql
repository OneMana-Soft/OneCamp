-- Migration 596 (linear-24 H1): SLA timers. A project's admins set rules
-- (which tasks, by priority, label, type and where they came from, are
-- promised an answer within how long, counted on the project's business
-- hours or the calendar's), and each task a rule matches carries one timer.
-- Adds only; every statement can run twice.

-- A project's rules, first match by rank. A removed rule is kept (deleted_at)
-- so the timers it started still name it, and times nothing new.
CREATE TABLE IF NOT EXISTS sla_rules (
    "id"             uuid PRIMARY KEY,
    "project_uuid"   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    "name"           text NOT NULL,
    "priorities"     text[] NOT NULL DEFAULT '{}',
    "labels"         text[] NOT NULL DEFAULT '{}',
    "type_ids"       uuid[] NOT NULL DEFAULT '{}',
    "sources"        text[] NOT NULL DEFAULT '{}',
    "target_minutes" integer NOT NULL CHECK ("target_minutes" > 0 AND "target_minutes" <= 525600),
    "clock"          text NOT NULL DEFAULT 'business' CHECK ("clock" IN ('business', 'calendar')),
    "starts"         text NOT NULL DEFAULT 'arrival' CHECK ("starts" IN ('arrival', 'accept')),
    "rank"           integer NOT NULL DEFAULT 0,
    "created_by"     uuid REFERENCES users(id) ON DELETE SET NULL,
    "updated_by"     uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at"     TIMESTAMP WITH TIME ZONE,
    "deleted_by"     uuid REFERENCES users(id) ON DELETE SET NULL
);
-- Whole, not partial: it is also the index for the cascade from projects, and
-- the foreign-key guard counts a partial index only under "<col> IS NOT NULL".
CREATE INDEX IF NOT EXISTS idx_sla_rules_project ON sla_rules (project_uuid, rank);
-- Every foreign key has an index leading with it (deleting a user looks here).
CREATE INDEX IF NOT EXISTS idx_fk_sla_rules_created_by ON sla_rules (created_by) WHERE created_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_fk_sla_rules_updated_by ON sla_rules (updated_by) WHERE updated_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_fk_sla_rules_deleted_by ON sla_rules (deleted_by) WHERE deleted_by IS NOT NULL;

-- One timer per task, never deleted: a task that stops matching, is
-- cancelled or is deleted ends its timer (ended_at, ended_reason) and keeps
-- its start, so matching again resumes the same promise. state is how it
-- settled: running, met (done in time), missed (done late) or ended.
CREATE TABLE IF NOT EXISTS task_slas (
    "task_uuid"      uuid PRIMARY KEY,
    "project_uuid"   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    "rule_id"        uuid REFERENCES sla_rules(id) ON DELETE SET NULL,
    "source"         text NOT NULL DEFAULT '',
    "started_at"     TIMESTAMP WITH TIME ZONE NOT NULL,
    "due_at"         TIMESTAMP WITH TIME ZONE,
    "risk_at"        TIMESTAMP WITH TIME ZONE,
    "state"          text NOT NULL CHECK ("state" IN ('running', 'met', 'missed', 'ended')),
    "met_at"         TIMESTAMP WITH TIME ZONE,
    "warned_at"      TIMESTAMP WITH TIME ZONE,
    "breach_told_at" TIMESTAMP WITH TIME ZONE,
    "ended_at"       TIMESTAMP WITH TIME ZONE,
    "ended_reason"   text,
    "updated_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
-- The running timers, soonest due first (the alerts' sweep reads these).
CREATE INDEX IF NOT EXISTS idx_task_slas_running ON task_slas (due_at) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_fk_task_slas_project ON task_slas (project_uuid);
CREATE INDEX IF NOT EXISTS idx_fk_task_slas_rule ON task_slas (rule_id) WHERE rule_id IS NOT NULL;

-- A project's work week and zone, on its one settings row (593): hours in
-- business/WeekHours' shape, none for Monday to Friday all day; the zone is
-- the one its SLAs count in, set from the admin who turned them on, none for
-- UTC. Nullable columns with no default: metadata only.
ALTER TABLE project_settings ADD COLUMN IF NOT EXISTS "work_hours" jsonb;
ALTER TABLE project_settings ADD COLUMN IF NOT EXISTS "work_zone" text;
