-- Down migration 89: drop Agent Builder tables.
DROP TABLE IF EXISTS ai_agent_runs;
DROP TABLE IF EXISTS ai_agents;
