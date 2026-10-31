-- Forms that take files and forms that add table rows (package 9, R4b;
-- design/projects-vs-asana-jira/PROGRAM.md, "R4b"). 494 is a placeholder: the
-- integrator gives it the next free number above beta's top when it lands.
--
-- A visitor's file waits in form_uploads, against the claim their page holds,
-- until the form is sent: then it becomes an attachment of the project and is
-- attached to the task. A file nobody sent is removed a day later (the
-- forms.uploads.purge sweep). claim is the SHA-256 of the visitor's claim
-- token, never the token. No foreign key to the form: a deleted form's
-- waiting files are removed by the same sweep, objects and all.
--
-- A form with a target_table adds a row to that data table instead of making
-- a task; form_submissions.row_id is the row it added.
--
-- Postgres 12: every statement can run twice.

ALTER TABLE project_forms ADD COLUMN IF NOT EXISTS target_table uuid;
ALTER TABLE form_submissions ADD COLUMN IF NOT EXISTS row_id uuid;

CREATE TABLE IF NOT EXISTS form_uploads (
    "id"           uuid PRIMARY KEY,
    "form_id"      uuid NOT NULL,
    "claim"        varchar(64) NOT NULL,
    "question"     varchar(40) NOT NULL,
    "obj_key"      varchar NOT NULL,
    "file_name"    varchar NOT NULL,
    "content_type" varchar NOT NULL,
    "size"         bigint NOT NULL,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "claimed_at"   TIMESTAMP WITH TIME ZONE,
    -- How many sweeps couldn't remove its object, and when the sweep tries it
    -- again (helpers.Backoff), so one that keeps failing never holds the rest.
    "purge_tries"  int NOT NULL DEFAULT 0,
    "retry_at"     TIMESTAMP WITH TIME ZONE
);
CREATE INDEX IF NOT EXISTS idx_form_uploads_claim ON form_uploads (form_id, claim);
CREATE INDEX IF NOT EXISTS idx_form_uploads_created ON form_uploads (created_at);
