-- Migration 170: each agent action carries its agent's signature.
--
-- The action log records every effecting call before it is attempted, but a
-- row proved nothing about who wrote it: anyone with database access could
-- insert or rewrite one. Each agent now has an Ed25519 key DERIVED from the
-- server's AI_CONFIG_KEK and its own id (never stored), and signs the intent
-- as it is recorded. Someone holding only the database cannot produce a valid
-- signature, and verifying needs no key column an attacker could replace.
--
-- Nullable: rows written before this migration are simply unsigned.
ALTER TABLE ai_agent_action_log
    ADD COLUMN IF NOT EXISTS signature bytea;
