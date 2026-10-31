-- B5c-2 (airtable-07's branding): a form's cover and logo, each an attachment one of
-- its managers uploaded for it (business/Form look.go). A deleted attachment leaves
-- the form without it.
ALTER TABLE project_forms
    ADD COLUMN IF NOT EXISTS cover_id uuid REFERENCES attachments(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS logo_id uuid REFERENCES attachments(id) ON DELETE SET NULL;
