-- Migration 108: per-agent knowledge sources.
--
-- An agent can be grounded on a curated set of references (channels, docs,
-- projects) it should always read for context — the "my agent actually knows my
-- stuff" capability buyers compare against Notion/Asana. Stored as a jsonb array
-- of { "type": "...", "id": "...", "label": "..." }. At run time the runner
-- pulls grounding from these sources AS THE OWNER, with permissions re-checked,
-- skipping any the owner cannot access. Additive + non-null with a safe empty
-- default, so every existing agent is unchanged.

ALTER TABLE ai_agents ADD COLUMN IF NOT EXISTS "knowledge" jsonb NOT NULL DEFAULT '[]'::jsonb;
