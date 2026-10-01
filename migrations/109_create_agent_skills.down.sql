-- Migration 109 down: drop reusable skills.
ALTER TABLE ai_agents DROP COLUMN IF EXISTS "skill_ids";
DROP TABLE IF EXISTS ai_agent_skills;
