-- Migration 108 down: drop the per-agent knowledge sources column.
ALTER TABLE ai_agents DROP COLUMN IF EXISTS "knowledge";
