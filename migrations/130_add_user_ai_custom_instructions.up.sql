-- Migration 130: per-user custom AI instructions (personal-agent guidance).
--
-- Notion/ChatGPT-style "custom instructions": free-text a member sets to shape
-- how the assistant answers THEM (tone, role, defaults, preferred language),
-- applied on top of the workspace system prompt for that user only. Stored on
-- the existing per-user AI preferences row so all personal AI settings live in
-- one place (keyed by user_uuid, independent of whether a model pick is set).
--
-- Additive + nullable-safe with an empty default: existing rows and the default
-- assistant behavior are unchanged when it is blank.
ALTER TABLE user_ai_model_preference
    ADD COLUMN IF NOT EXISTS custom_instructions text NOT NULL DEFAULT '';
