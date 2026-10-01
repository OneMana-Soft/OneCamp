-- Rollback migration 133: drop the admin-managed code-PR coding wall limit.
-- The server falls back to the AI_CODE_PR_WALL_MINUTES env override, then its
-- built-in default, so a rollback degrades cleanly rather than losing the limit.
ALTER TABLE ai_settings
    DROP COLUMN IF EXISTS code_pr_wall_minutes;
