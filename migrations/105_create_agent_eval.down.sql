-- Migration 105 down: drop the agent evaluation harness tables.
DROP TABLE IF EXISTS ai_agent_eval_results;
DROP TABLE IF EXISTS ai_agent_eval_scenarios;
