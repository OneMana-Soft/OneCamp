-- Rules that run work by themselves (package 9, R1; design/projects-vs-asana-jira/PROGRAM.md §16).
--
-- The Workflow engine (migrations 76, 77, 145) becomes the one rules engine:
-- a rule is a trigger, conditions, the actions when they hold and, optionally,
-- the actions when they don't. A rule may belong to a project, where the
-- project's admins keep it and every member sees it. Each run is a row: its
-- event key makes a run happen once however often the event is delivered, and
-- its steps keep each change's before and after so a run can be undone.

ALTER TABLE workflows ADD COLUMN IF NOT EXISTS project_id uuid;
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS conditions jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS else_actions jsonb NOT NULL DEFAULT '[]'::jsonb;
-- Why a rule stopped itself (too many runs), shown on the rule; cleared when
-- someone turns it back on.
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS paused_reason text;
-- Round robin's turn: taken with UPDATE ... RETURNING, so two runs at once
-- give two different people.
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS rr_cursor bigint NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_workflows_project
    ON workflows (project_id)
    WHERE deleted_at IS NULL AND project_id IS NOT NULL;

ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_trigger_type_check;
ALTER TABLE workflows ADD CONSTRAINT workflows_trigger_type_check
    CHECK (trigger_type IN (
        'message_posted',
        'reaction_added',
        'user_joined_channel',
        'task_created',
        'task_status_changed',
        'meeting_ended',
        'task_assigned',
        'task_field_changed'
    ));

CREATE TABLE IF NOT EXISTS workflow_runs (
    "id"          uuid PRIMARY KEY,
    "workflow_id" uuid NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    -- The event's own id (or its trigger and subject): a second delivery of
    -- the same event finds its run already claimed.
    "event_key"   varchar(200) NOT NULL,
    "trigger"     varchar(40) NOT NULL,
    "project_id"  uuid,
    -- What the run acted on: a task, a message, a channel.
    "subject_id"  varchar(64),
    "subject"     varchar(300),
    -- running, ok, failed, skipped, undone
    "status"      varchar(16) NOT NULL DEFAULT 'running',
    -- Which branch ran: then or else.
    "branch"      varchar(8),
    "error"       text,
    -- [{type, status, detail, task_id, before, after}]
    "steps"       jsonb NOT NULL DEFAULT '[]'::jsonb,
    "started_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "finished_at" TIMESTAMP WITH TIME ZONE,
    "undone_by"   uuid,
    "undone_at"   TIMESTAMP WITH TIME ZONE
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_workflow_runs_event ON workflow_runs (workflow_id, event_key);
-- A rule's history, newest first, and its rate (runs in the last minute/hour).
CREATE INDEX IF NOT EXISTS idx_workflow_runs_rule_started ON workflow_runs (workflow_id, started_at DESC);
-- A project's history across its rules, and the workspace's hourly rate.
CREATE INDEX IF NOT EXISTS idx_workflow_runs_project_started ON workflow_runs (project_id, started_at DESC) WHERE project_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_workflow_runs_started ON workflow_runs (started_at);
