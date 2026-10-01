-- Migration 109: reusable agent skills.
--
-- A skill is a named, reusable instruction module a team defines once (e.g.
-- "How we write status updates") and attaches to many agents. Editing a skill
-- updates every agent that references it on its next run (no per-agent copy
-- drift). Agents reference skills by id via an additive skill_ids jsonb array.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS ai_agent_skills (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "name"         varchar NOT NULL,
    "instructions" text NOT NULL,
    "created_by"   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "created_at"   TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"   TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at"   TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_ai_agent_skills_active
    ON ai_agent_skills (created_at DESC)
    WHERE deleted_at IS NULL;

-- Agents reference skills by id (composed into the system prompt at run time).
ALTER TABLE ai_agents ADD COLUMN IF NOT EXISTS "skill_ids" jsonb NOT NULL DEFAULT '[]'::jsonb;
