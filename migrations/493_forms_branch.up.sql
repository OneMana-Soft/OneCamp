-- Forms that branch and fill fields (package 9, R4; design/projects-vs-asana-jira/
-- PROGRAM.md, "R4 plan"). 493 is a placeholder: the integrator gives it the
-- next free number above beta's top when it lands.
--
-- Questions keep living in project_forms.fields (jsonb): a section, a
-- question's help, when it shows (show_if) and what it fills (maps_to) are
-- keys of each item, checked in business/Form. What a visitor sees once the
-- form is sent is the form's own words now.
--
-- A project rule can run when one of its forms is sent (trigger_config
-- form_id).
--
-- Postgres 12: every statement can run twice.

ALTER TABLE project_forms ADD COLUMN IF NOT EXISTS confirmation text NOT NULL DEFAULT '';

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
        'form_submitted'
    ));
