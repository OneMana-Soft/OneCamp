-- Migration 931 (a placeholder, numbered when it lands): tables B5b, a form
-- may send its visitor to a page of the team's once it's sent (Airtable's
-- redirect), in place of its message. '' keeps the message. Postgres 12:
-- every statement can run twice.
ALTER TABLE project_forms ADD COLUMN IF NOT EXISTS redirect_url varchar NOT NULL DEFAULT '';
