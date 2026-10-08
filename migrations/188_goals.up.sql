-- Migration 188: goals. A goal is an outcome the team is after by a date,
-- with one owner. Its progress comes from the projects that serve it (how much
-- of their work is done), from its sub-goals, or from a number its owner
-- updates; check-ins say where it stands and keep its history. Closing one
-- (achieved, missed, dropped) keeps the progress it closed at.
CREATE TABLE IF NOT EXISTS goals (
    "id"             uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "title"          varchar(200) NOT NULL CHECK (char_length(btrim(title)) BETWEEN 1 AND 200),
    "description"    text NOT NULL DEFAULT '' CHECK (char_length(description) <= 4000),
    "owner_uuid"     uuid NOT NULL,
    "created_by"     uuid NOT NULL,
    "parent_id"      uuid REFERENCES goals (id),
    "start_date"     date,
    "due_date"       date NOT NULL,
    "measure"        varchar(16) NOT NULL CHECK (measure IN ('projects', 'subgoals', 'number')),
    "start_value"    double precision,
    "target_value"   double precision,
    "current_value"  double precision,
    "unit"           varchar(24) NOT NULL DEFAULT '',
    "status"         varchar(16) NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'achieved', 'missed', 'dropped')),
    "final_progress" double precision,
    "closed_at"      TIMESTAMP WITH TIME ZONE,
    "created_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at"     TIMESTAMP WITH TIME ZONE,
    CHECK (start_date IS NULL OR start_date <= due_date),
    CHECK (parent_id IS NULL OR parent_id <> id),
    -- A number goal knows where it started, where it's going and where it is.
    CHECK (measure <> 'number' OR (start_value IS NOT NULL AND target_value IS NOT NULL AND current_value IS NOT NULL AND start_value <> target_value))
);
CREATE INDEX IF NOT EXISTS goals_live ON goals (status, due_date) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS goals_parent ON goals (parent_id) WHERE deleted_at IS NULL;

-- The projects that serve a goal. A project may serve several goals.
CREATE TABLE IF NOT EXISTS goal_projects (
    "goal_id"      uuid NOT NULL REFERENCES goals (id) ON DELETE CASCADE,
    "project_uuid" uuid NOT NULL,
    "added_by"     uuid NOT NULL,
    "added_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (goal_id, project_uuid)
);
CREATE INDEX IF NOT EXISTS goal_projects_project ON goal_projects (project_uuid);

-- A goal's check-ins: where it stands, a note, and for a number goal the value
-- it was moved to. progress is the goal's progress when it was posted, so the
-- history can say how far it moved between check-ins. A closing check-in's
-- health is how the goal ended.
CREATE TABLE IF NOT EXISTS goal_checkins (
    "id"          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "goal_id"     uuid NOT NULL REFERENCES goals (id) ON DELETE CASCADE,
    "author_uuid" uuid NOT NULL,
    "health"      varchar(16) NOT NULL CHECK (health IN ('on_track', 'at_risk', 'off_track', 'on_hold', 'achieved', 'missed', 'dropped')),
    "body"        text NOT NULL DEFAULT '' CHECK (char_length(body) <= 8000),
    "value"       double precision,
    "progress"    double precision,
    "created_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at"  TIMESTAMP WITH TIME ZONE
);
CREATE INDEX IF NOT EXISTS goal_checkins_goal ON goal_checkins (goal_id, created_at DESC) WHERE deleted_at IS NULL;
