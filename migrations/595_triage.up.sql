-- Migration 595: the Triage Queue
-- (package 9 R5a; PROGRAM.md §4.2 #91, D-PJ-5 as a mark beside the status).
-- A project with triage on (the projects.triage policy, set for the project
-- or the workspace) takes the work that comes in from outside its board (a
-- form, the API, an agent, GitHub, a chat, an import) into triage until one
-- of its people decides it: accept it as the team's work, mark it a
-- duplicate, decline it or snooze it. What
-- waits leaves the project's lists and counts (models/taskread) and is hidden
-- from no one; the task node in the graph is unchanged.
--
-- Postgres 12: every statement can run twice. Only adds: a server from
-- before this keeps working on it.

-- What waits, and what triage has decided. A row stays once decided, so a
-- task goes through triage once.
CREATE TABLE IF NOT EXISTS triage_items (
    "task_uuid"       uuid PRIMARY KEY,
    "project_uuid"    uuid NOT NULL,
    "source"          varchar(8) NOT NULL CHECK (source IN ('form', 'api', 'agent', 'github', 'chat', 'import')),
    -- Who asked: a member, or a form's visitor by the name and email they gave.
    "requester_uuid"  uuid,
    "requester_name"  varchar(200) NOT NULL DEFAULT '',
    "requester_email" varchar(320) NOT NULL DEFAULT '',
    "arrived_at"      timestamptz NOT NULL DEFAULT NOW(),
    -- Snoozed: still waiting, out of the queue until then.
    "snoozed_until"   timestamptz,
    "decided_at"      timestamptz
);

-- The queue and the lists' filter read only what waits.
CREATE INDEX IF NOT EXISTS triage_items_waiting_idx ON triage_items (project_uuid, arrived_at) WHERE decided_at IS NULL;

-- Every decision, with its reason: insert-only.
CREATE TABLE IF NOT EXISTS triage_decisions (
    "id"            bigserial PRIMARY KEY,
    "task_uuid"     uuid NOT NULL,
    "project_uuid"  uuid NOT NULL,
    "decision"      varchar(16) NOT NULL CHECK (decision IN ('accept', 'duplicate', 'decline', 'snooze')),
    "reason"        varchar(500) NOT NULL DEFAULT '',
    "duplicate_of"  uuid,
    "snoozed_until" timestamptz,
    "decided_by"    uuid NOT NULL,
    "decided_at"    timestamptz NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS triage_decisions_task_idx ON triage_decisions (task_uuid, decided_at);
