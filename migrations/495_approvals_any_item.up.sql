-- Migration 495 (a placeholder; the integrator numbers it): approvals on any
-- item (package 9 R2b, with basecamp-25's H-5). One approval model in R2's
-- store (491):
-- - A request is about a task, a doc or a file (resource_kind, resource_id).
--   One with to_status holds a task's move (StatusGate, as before); one
--   without asks for the item itself to be approved, with what is asked, an
--   optional due time and the reminder that went.
-- - A client answers through a link the request names (client_grants),
--   never as a user. A decision is a member's (user_id) or a link's
--   (grant_id, with the name the client gave), exactly one.
-- - A decision on a doc keeps the body it decided on (body_hash, version), so
--   a later change can say "Changed since approved".
-- - A status rule can want the project's clients too (status_rules.client).
-- - client_reviews gains one resource_uuid for every kind; task_uuid and
--   doc_uuid stay for a release beside it.
-- - Rules can start when a request is answered (approval_decided).
-- Every change is a new column with a default or a dropped NOT NULL: no table
-- rewrite on Postgres 12. The API that runs between this migration and its
-- own restart still works: it writes no resource_id or resource_uuid (both
-- may be empty for a task, and reads fall back to task_id), and its
-- ON CONFLICT (approval_id, user_id) still finds a unique index.

ALTER TABLE approvals ADD COLUMN IF NOT EXISTS resource_kind varchar(8) NOT NULL DEFAULT 'task';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS resource_id varchar(64);
UPDATE approvals SET resource_id = task_id::text WHERE resource_id IS NULL;
ALTER TABLE approvals ALTER COLUMN task_id DROP NOT NULL;
ALTER TABLE approvals ALTER COLUMN project_id DROP NOT NULL;
ALTER TABLE approvals ALTER COLUMN to_status DROP NOT NULL;
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS ask varchar(500) NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS due_at timestamptz;
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS reminded_at timestamptz;
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS client_grants uuid[] NOT NULL DEFAULT '{}';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'approvals_resource') THEN
        ALTER TABLE approvals ADD CONSTRAINT approvals_resource CHECK (
            resource_kind IN ('task', 'doc', 'file')
            AND (resource_kind <> 'task' OR (task_id IS NOT NULL AND project_id IS NOT NULL AND (resource_id IS NULL OR resource_id = task_id::text)))
            AND (resource_kind = 'task' OR resource_id IS NOT NULL)
            AND (to_status IS NULL OR resource_kind = 'task'));
    END IF;
END $$;

-- One open request per item, of any kind.
DROP INDEX IF EXISTS uq_approvals_pending_task;
CREATE UNIQUE INDEX IF NOT EXISTS uq_approvals_pending_resource ON approvals (resource_kind, (COALESCE(resource_id, task_id::text))) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS idx_approvals_resource ON approvals (resource_kind, (COALESCE(resource_id, task_id::text)), created_at DESC);
-- The reminder pass reads open requests by their due time.
CREATE INDEX IF NOT EXISTS idx_approvals_due ON approvals (due_at) WHERE state = 'pending' AND due_at IS NOT NULL;

ALTER TABLE approval_decisions ADD COLUMN IF NOT EXISTS grant_id uuid REFERENCES guest_grants(id) ON DELETE CASCADE;
ALTER TABLE approval_decisions ADD COLUMN IF NOT EXISTS name varchar(60) NOT NULL DEFAULT '';
ALTER TABLE approval_decisions ADD COLUMN IF NOT EXISTS body_hash varchar(64) NOT NULL DEFAULT '';
ALTER TABLE approval_decisions ADD COLUMN IF NOT EXISTS version int;
ALTER TABLE approval_decisions DROP CONSTRAINT IF EXISTS approval_decisions_pkey;
ALTER TABLE approval_decisions ALTER COLUMN user_id DROP NOT NULL;
-- Not partial, so ON CONFLICT (approval_id, user_id) still finds it; a
-- link's decisions have no user_id, and nulls never collide.
CREATE UNIQUE INDEX IF NOT EXISTS uq_approval_decisions_user ON approval_decisions (approval_id, user_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_approval_decisions_grant ON approval_decisions (approval_id, grant_id) WHERE grant_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_approval_decisions_grant_id ON approval_decisions (grant_id);
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'approval_decisions_by') THEN
        ALTER TABLE approval_decisions ADD CONSTRAINT approval_decisions_by CHECK ((user_id IS NULL) <> (grant_id IS NULL));
    END IF;
END $$;

ALTER TABLE status_rules ADD COLUMN IF NOT EXISTS client boolean NOT NULL DEFAULT false;

ALTER TABLE client_reviews ADD COLUMN IF NOT EXISTS resource_uuid varchar(64);
UPDATE client_reviews SET resource_uuid = COALESCE(task_uuid::text, doc_uuid) WHERE resource_uuid IS NULL;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'client_reviews_resource_uuid') THEN
        ALTER TABLE client_reviews ADD CONSTRAINT client_reviews_resource_uuid CHECK (
            resource_uuid IS NULL OR resource_uuid = COALESCE(task_uuid::text, doc_uuid));
    END IF;
END $$;

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
        'task_field_changed',
        'schedule',
        'task_due_soon',
        'task_overdue',
        'form_submitted',
        'approval_decided'
    ));
