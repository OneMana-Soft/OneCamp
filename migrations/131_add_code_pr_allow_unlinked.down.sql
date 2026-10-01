-- Rollback migration 131: drop the code-PR "work on any accessible repo" toggle.
ALTER TABLE ai_settings
    DROP COLUMN IF EXISTS code_pr_allow_unlinked;
