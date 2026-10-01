-- Migration 66: team-report ambient agent toggle.
--
-- Adds the opt-in flag for the memory-grounded weekly "state of the
-- channel" report agent. Defaults OFF so enabling the memory layer does
-- not silently start posting into channels — an admin opts in explicitly.
-- Idempotent: safe to re-run.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS "team_report_enabled" boolean NOT NULL DEFAULT false;
