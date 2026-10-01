-- Rollback migration 120: drop agent routines.
DROP INDEX IF EXISTS idx_agent_routines_channel;
DROP INDEX IF EXISTS idx_agent_routines_agent;
DROP INDEX IF EXISTS idx_agent_routines_due;
DROP TABLE IF EXISTS agent_routines;
