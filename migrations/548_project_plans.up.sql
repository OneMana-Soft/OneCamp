-- Migration 548: a project's plan
-- (linear-15). Who leads a project, when it starts and when it should land
-- (each at the resolution its people chose: a day, a month, a quarter, a half
-- or a year), how much it matters, its labels and where it stands, read on
-- the project's home, the projects overview and the projects timeline
-- (business/ProjectPlan). Every plan, every project.

-- The workspace's own project statuses, each in one of the five categories
-- every project status falls in. The five built-in statuses, one per
-- category, live in code (business/ProjectPlan); these are the ones a
-- workspace adds beside them ("In review" under in progress). A delete is
-- soft (V-78): a project in a deleted status reads as its category's
-- built-in, and a restore brings the status back to it.
CREATE TABLE IF NOT EXISTS project_statuses (
    "id"         uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "name"       varchar(40) NOT NULL CHECK (btrim(name) <> ''),
    "category"   varchar(16) NOT NULL CHECK (category IN ('planned', 'in_progress', 'paused', 'done', 'cancelled')),
    "color"      varchar(16) NOT NULL DEFAULT 'slate',
    "position"   int NOT NULL DEFAULT 0,
    "created_by" uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE,
    "deleted_by" uuid
);
-- Each live name once, whatever its case; a restore whose name a live status
-- has taken since is refused.
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_statuses_live_name ON project_statuses (lower(name)) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_project_statuses_created_by ON project_statuses (created_by);

-- The workspace's project labels ("Q4 launch", "Customer"), a project
-- carrying any number of them. A delete is soft (V-78).
CREATE TABLE IF NOT EXISTS project_labels (
    "id"         uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "name"       varchar(40) NOT NULL CHECK (btrim(name) <> ''),
    "color"      varchar(16) NOT NULL DEFAULT 'slate',
    "created_by" uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE,
    "deleted_by" uuid
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_labels_live_name ON project_labels (lower(name)) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_project_labels_created_by ON project_labels (created_by);

-- One row per project whose plan anyone has set; a project with none reads
-- as in progress, with no lead, dates, priority or labels.
CREATE TABLE IF NOT EXISTS project_plans (
    "project_id"        uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    "lead_id"           uuid REFERENCES users(id) ON DELETE SET NULL,
    -- A start is the first day of its period and a target the last, so a
    -- quarter's target is its last day and sorts after every day inside it.
    "start_on"          date,
    "start_resolution"  varchar(8) CHECK (start_resolution IN ('day', 'month', 'quarter', 'half', 'year')),
    "target_on"         date,
    "target_resolution" varchar(8) CHECK (target_resolution IN ('day', 'month', 'quarter', 'half', 'year')),
    "priority"          varchar(8) CHECK (priority IN ('urgent', 'high', 'medium', 'low')),
    "status_category"   varchar(16) NOT NULL DEFAULT 'in_progress' CHECK (status_category IN ('planned', 'in_progress', 'paused', 'done', 'cancelled')),
    -- A workspace status in that category, or none: the category's built-in.
    "status_id"         uuid REFERENCES project_statuses(id) ON DELETE SET NULL,
    "updated_by"        uuid,
    "updated_at"        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CHECK ((start_on IS NULL) = (start_resolution IS NULL)),
    CHECK ((target_on IS NULL) = (target_resolution IS NULL)),
    CHECK (start_on IS NULL OR target_on IS NULL OR start_on <= target_on)
);
CREATE INDEX IF NOT EXISTS idx_project_plans_lead_id ON project_plans (lead_id);
CREATE INDEX IF NOT EXISTS idx_project_plans_status_id ON project_plans (status_id);

-- The labels a project carries.
CREATE TABLE IF NOT EXISTS project_plan_labels (
    "project_id" uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    "label_id"   uuid NOT NULL REFERENCES project_labels(id) ON DELETE CASCADE,
    "added_by"   uuid,
    "added_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, label_id)
);
CREATE INDEX IF NOT EXISTS idx_project_plan_labels_label_id ON project_plan_labels (label_id);
