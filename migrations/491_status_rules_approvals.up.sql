-- Status rules and approvals (package 9 R2; design/projects-vs-asana-jira/PROGRAM.md §16).
--
-- A project's status may have a rule: who may move a task into it, what the
-- task must have first, and who must approve the move. Every status change
-- passes it (business/Task UpdateTaskStatusByTaskUUID: the app, the API,
-- boards, GitHub, agents and rules alike). A move into a status that needs
-- approval becomes a request; the move is made, as the person who asked, once
-- enough approvers say yes.

CREATE TABLE IF NOT EXISTS status_rules (
    "project_id"       uuid NOT NULL,
    -- A built-in status key (it covers the project's own statuses counting as
    -- it) or one of the project's own statuses' ids.
    "status"           varchar(64) NOT NULL,
    -- anyone | admins | people
    "who"              varchar(16) NOT NULL DEFAULT 'anyone',
    "people"           uuid[] NOT NULL DEFAULT '{}',
    -- What the task must have: assignee, due, tags, description.
    "required"         text[] NOT NULL DEFAULT '{}',
    "approvers"        uuid[] NOT NULL DEFAULT '{}',
    -- How many approvers must say yes (1 .. the number of approvers).
    "approvals_needed" int NOT NULL DEFAULT 1,
    "updated_by"       uuid NOT NULL,
    "updated_at"       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, status),
    CHECK (who IN ('anyone', 'admins', 'people')),
    CHECK (approvals_needed >= 1)
);

CREATE TABLE IF NOT EXISTS approvals (
    "id"           uuid PRIMARY KEY,
    "project_id"   uuid NOT NULL,
    "task_id"      uuid NOT NULL,
    -- The status the task goes to when it's approved.
    "to_status"    varchar(64) NOT NULL,
    "requested_by" uuid NOT NULL,
    "approvers"    uuid[] NOT NULL,
    "needed"       int NOT NULL,
    -- pending | approved | changes | rejected | withdrawn
    "state"        varchar(16) NOT NULL DEFAULT 'pending',
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "decided_at"   TIMESTAMP WITH TIME ZONE,
    CHECK (state IN ('pending', 'approved', 'changes', 'rejected', 'withdrawn'))
);

-- One open request per task: a second move waits on the first.
CREATE UNIQUE INDEX IF NOT EXISTS uq_approvals_pending_task ON approvals (task_id) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS idx_approvals_task ON approvals (task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_approvals_project_pending ON approvals (project_id, created_at DESC) WHERE state = 'pending';

CREATE TABLE IF NOT EXISTS approval_decisions (
    "approval_id" uuid NOT NULL REFERENCES approvals(id) ON DELETE CASCADE,
    "user_id"     uuid NOT NULL,
    -- approve | changes | reject
    "verdict"     varchar(16) NOT NULL,
    "comment"     text NOT NULL DEFAULT '',
    "decided_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (approval_id, user_id),
    CHECK (verdict IN ('approve', 'changes', 'reject'))
);
