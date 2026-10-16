ALTER TABLE ai_pending_actions
    DROP COLUMN IF EXISTS "tool_binding",
    DROP COLUMN IF EXISTS "asked_by";
