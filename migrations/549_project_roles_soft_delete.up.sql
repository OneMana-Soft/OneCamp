-- V-184 head 3 (the owner's rule: every delete is soft): taking a person off a
-- project, taking them off the team whose project it is, and the parity sweep
-- settling a project's roles mark the person's project_roles row, when and by
-- whom (NULL for the sweep), instead of removing it, so who held which role,
-- and who took it away and when, stays on record. Every read of the table
-- takes live rows only (models/postgres/SoftDelete.Live). A role given again
-- brings its own row back: one row per project and person, as before, so the
-- key stays.
ALTER TABLE project_roles ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
