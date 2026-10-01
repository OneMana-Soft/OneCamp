-- Migration 83: opt-in auto-triage of new GitHub issues.
--
-- When enabled, a newly-opened GitHub issue on a linked repo is analysed by
-- the code-aware bug agent and the proposed root-cause + fix is posted as an
-- internal comment on the auto-created task (never pushed back to GitHub).
--
-- Defaults OFF because it spends an LLM call on every opened issue: an admin
-- opts in knowingly, exactly like the other ambient agents (meeting recap,
-- team report, nudges). Idempotent: safe to re-run.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS "issue_triage_enabled" boolean NOT NULL DEFAULT false;
