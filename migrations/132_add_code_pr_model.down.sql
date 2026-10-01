-- Rollback migration 132: drop the optional dedicated code-PR model selection.
ALTER TABLE ai_settings
    DROP COLUMN IF EXISTS code_pr_chat_provider_id,
    DROP COLUMN IF EXISTS code_pr_chat_model;
