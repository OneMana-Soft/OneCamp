-- Bind an API credential to an AGENT IDENTITY.
--
-- WHY. A token id in an audit row is not an answer to "who did this". Non-human
-- identity guidance is explicit that a record showing only a token or a service
-- account is too weak to establish intent, ownership or containment — good
-- auditability is identity plus context. And the identity vendors converged during
-- 2026 on agents as a FIRST-CLASS identity class: not a human to impersonate, not a
-- shared service account to hide inside. Microsoft's agent identities are a distinct
-- object type that deliberately hold no credentials of their own.
--
-- That last point is the shape this migration copies. An agent is an IDENTITY; a
-- token is a CREDENTIAL. They are separate rows because the relationship is
-- one-to-many and their lifecycles differ: a credential is rotated, an identity
-- persists; a credential is revoked, an identity is retired. Putting the agent on
-- the token rather than the reverse is what allows one agent to hold several
-- credentials (a rotation window, one per client) while every one of them resolves
-- to the same actor in the audit trail.
--
-- WHAT THIS BUYS, concretely:
--
--   * attribution — the audit row names the agent, not a hex string
--   * an INDEPENDENT KILL SWITCH — ai_agents.is_active already exists, so
--     deactivating an agent stops every credential bound to it at once, without
--     hunting for tokens to revoke. Checked per call, so it takes effect on the
--     next request rather than at the next rotation.
--   * a clear owner — ai_agents.created_by, alongside the token's own created_by
--
-- NULLABLE, deliberately. Every token that exists today is a plain integration
-- credential and stays one; this is not a migration that reinterprets existing rows.
-- An unbound token keeps working exactly as before and audits as an api_client
-- rather than an agent, which is itself the honest answer for a script.
--
-- ON DELETE SET NULL rather than CASCADE: deleting an agent must not silently
-- destroy a credential and break an integration. The token survives, loses its agent
-- attribution, and an operator can see it needs attention.
ALTER TABLE api_tokens
    ADD COLUMN IF NOT EXISTS "agent_id" uuid REFERENCES ai_agents(id) ON DELETE SET NULL;

-- Partial index: the lookup is always "which tokens belong to this agent", asked
-- when an agent is deactivated or reviewed. Unbound tokens are the majority and are
-- never the subject of that question, so they are excluded.
CREATE INDEX IF NOT EXISTS idx_api_tokens_agent
    ON api_tokens ("agent_id")
    WHERE agent_id IS NOT NULL;

COMMENT ON COLUMN api_tokens.agent_id IS
    'Optional agent identity this credential authenticates. NULL means a plain '
    'integration credential. When set, the agent is the actor in audit records and '
    'ai_agents.is_active acts as an independent kill switch checked on every call.';
