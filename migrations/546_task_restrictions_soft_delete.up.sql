-- V-179 (the owner's rule: every delete is soft): taking a person off a
-- restricted task, lifting a task's restriction (with the tasks restricted
-- with it) and a person's leaving the task's project mark the rows, when and
-- by whom, instead of removing them, so who was let in, and who took them off
-- and when, stays on record. Every read of these tables takes live rows only
-- (models/postgres/SoftDelete.Live). A task restricted again, or a person let
-- in again, brings its own row back: one row per task, and per task and
-- person, as before, so the keys and the foreign keys on them stay.
ALTER TABLE task_restrictions       ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
ALTER TABLE task_restriction_grants ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
