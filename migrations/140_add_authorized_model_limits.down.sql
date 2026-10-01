-- Reverting drops the per-model limits, returning every model to the single workspace
-- context window (ai_settings.context_window_tokens).
--
-- Nothing breaks: 0 already meant "inherit the workspace value", so removing the columns
-- puts every model back on the path it would have taken with them empty. What is LOST is
-- the operator's knowledge — a 128k model an admin had correctly described goes back to
-- being budgeted as though it were the workspace default, quietly. Re-applying the
-- migration restores the columns but not the values.
ALTER TABLE ai_authorized_models
    DROP CONSTRAINT IF EXISTS ai_authorized_models_context_window_sane,
    DROP CONSTRAINT IF EXISTS ai_authorized_models_max_output_sane,
    DROP COLUMN IF EXISTS context_window_tokens,
    DROP COLUMN IF EXISTS max_output_tokens;
