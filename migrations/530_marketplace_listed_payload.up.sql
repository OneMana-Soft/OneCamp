-- A template's listed form, kept when it is saved: a board's drawing (a title,
-- a count and up to 160 shapes) and a doc's preview, in place of a payload
-- that may be a megabyte. The Templates list reads only this column (audit
-- C11). NULL is a template saved before the column, which the server fills
-- from its payload once it starts (business/Marketplace StartListedFormBackfill)
-- and lists from its payload until then. No default, so adding it rewrites
-- nothing on Postgres 12.
ALTER TABLE marketplace_templates ADD COLUMN IF NOT EXISTS listed_payload jsonb;
