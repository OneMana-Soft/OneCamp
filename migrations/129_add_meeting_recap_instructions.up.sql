-- Migration 129: admin-configurable meeting-recap instructions.
--
-- The post-call recap agent (business/AI/meetingRecapAgent.go) uses a fixed
-- system prompt. Notion-style meeting notes let a workspace tailor what the
-- recap emphasizes (e.g. "always add a Risks section", "write in Spanish",
-- "flag anything needing legal review"). This column stores that free-text
-- guidance, appended to the base recap prompt when non-empty.
--
-- Additive + nullable with a safe empty default: existing rows and the default
-- recap behavior are unchanged when it is blank.
ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS meeting_recap_instructions text NOT NULL DEFAULT '';
