-- Reversing this loses agent attribution on existing credentials but breaks no
-- integration: an unbound token authenticates exactly as a bound one did, it simply
-- audits as an api_client rather than as a named agent.
DROP INDEX IF EXISTS idx_api_tokens_agent;

ALTER TABLE api_tokens
    DROP COLUMN IF EXISTS "agent_id";
