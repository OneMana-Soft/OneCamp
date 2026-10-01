-- Migration 131: code-PR "work on any accessible repo" admin setting.
--
-- Adds an admin-managed toggle to the singleton ai_settings row that controls
-- whether the code-PR agent may open a PR on ANY repository the connected
-- GitHub account can reach (verified per-run), or ONLY repositories linked to a
-- project (the conservative default).
--
-- Default FALSE (linked-only) — the blast-radius-safe default for shared /
-- multi-tenant installs, where the connected token may see many repos. An admin
-- opts in explicitly. The env var AI_CODE_PR_ALLOW_UNLINKED remains a boot-time
-- override for automated deployments. Idempotent.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS code_pr_allow_unlinked boolean NOT NULL DEFAULT false;
