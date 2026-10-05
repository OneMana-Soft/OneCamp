-- Migration 179: intake forms (Asana's forms). A project's admins publish a
-- form at /f/<token>; anyone can fill it in, and each answer set becomes a
-- task in the project. Fields are JSON: [{id, label, type, required, options}].
CREATE TABLE IF NOT EXISTS project_forms (
    "id"            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "project_uuid"  uuid NOT NULL,
    "token"         varchar NOT NULL UNIQUE,
    "title"         varchar NOT NULL,
    "description"   text NOT NULL DEFAULT '',
    "fields"        jsonb NOT NULL,
    "title_field"   varchar NOT NULL DEFAULT '',
    "priority"      varchar NOT NULL DEFAULT 'medium',
    "assignee_uuid" uuid,
    "active"        boolean NOT NULL DEFAULT true,
    "created_by"    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "created_at"    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_project_forms_project ON project_forms(project_uuid);

CREATE TABLE IF NOT EXISTS form_submissions (
    "id"         uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "form_id"    uuid NOT NULL REFERENCES project_forms(id) ON DELETE CASCADE,
    "task_uuid"  uuid,
    "answers"    jsonb NOT NULL,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_form_submissions_form ON form_submissions(form_id, created_at);
