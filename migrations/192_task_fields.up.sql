-- Migration 192: custom fields on tasks. A project's admins give its tasks
-- their own fields (a budget, a channel, a reviewer); each task holds at most
-- one value per field. The field's type says what a value is:
--   text, url      a JSON string
--   number         a JSON number
--   money          a JSON integer in hundredths of the currency's unit (cents,
--                  paise), whatever the currency, as project_billing keeps rates
--   date           a JSON string "YYYY-MM-DD": a day, in no time zone
--   select         a JSON string: the id of one of the field's options
--   multi_select   a JSON array of option ids
--   person         a JSON string: a user's id
--   checkbox       JSON true (an unticked box has no value)
-- Options are kept by id, so renaming one changes it on every task.
-- See business/TaskField.
CREATE TABLE IF NOT EXISTS task_fields (
    "id"         uuid PRIMARY KEY,
    "project_id" uuid NOT NULL,
    "name"       varchar(40) NOT NULL,
    "type"       varchar(16) NOT NULL CHECK (type IN ('text', 'number', 'money', 'date', 'select', 'multi_select', 'person', 'checkbox', 'url')),
    "options"    jsonb NOT NULL DEFAULT '[]'::jsonb,
    "currency"   char(3) CHECK (currency ~ '^[A-Z]{3}$'),
    "on_card"    boolean NOT NULL DEFAULT false,
    "position"   integer NOT NULL DEFAULT 0,
    "created_by" uuid,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- Names are unique within a project, whatever their case.
CREATE UNIQUE INDEX IF NOT EXISTS task_fields_project_name_idx ON task_fields (project_id, lower(name));
CREATE INDEX IF NOT EXISTS task_fields_project_idx ON task_fields (project_id, position);

CREATE TABLE IF NOT EXISTS task_field_values (
    "task_uuid"  uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    "field_id"   uuid NOT NULL REFERENCES task_fields(id) ON DELETE CASCADE,
    "value"      jsonb NOT NULL,
    "updated_by" uuid,
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (task_uuid, field_id)
);
-- Filters ask "which tasks have this value of this field".
CREATE INDEX IF NOT EXISTS task_field_values_field_idx ON task_field_values (field_id);
