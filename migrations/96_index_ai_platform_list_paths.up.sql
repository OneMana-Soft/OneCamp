-- Migration 96: index the recency-sorted management list paths added by the AI
-- platform (agents, workflows, tables) and the all-kinds template browse.
--
-- These lists all "ORDER BY <recency> DESC" over the non-deleted rows, but only
-- had indexes on created_by / (kind, created_at) / the active-trigger hot path.
-- The plain recency sort therefore fell back to a seq-scan + sort. Each partial
-- index below matches a real list query's filter+sort so it stays index-only as
-- the workspace grows. Idempotent.

-- data_tables: ListTablesVisibleTo ORDER BY updated_at DESC.
CREATE INDEX IF NOT EXISTS idx_data_tables_updated
    ON data_tables (updated_at DESC)
    WHERE deleted_at IS NULL;

-- ai_agents: ListAgents ORDER BY created_at DESC.
CREATE INDEX IF NOT EXISTS idx_ai_agents_created
    ON ai_agents (created_at DESC)
    WHERE deleted_at IS NULL;

-- workflows: ListWorkflows ORDER BY created_at DESC.
CREATE INDEX IF NOT EXISTS idx_workflows_created
    ON workflows (created_at DESC)
    WHERE deleted_at IS NULL;

-- marketplace_templates: the all-kinds browse ORDER BY created_at DESC (the
-- kind-filtered path is already served by idx_marketplace_templates_listed).
CREATE INDEX IF NOT EXISTS idx_marketplace_templates_created
    ON marketplace_templates (created_at DESC)
    WHERE deleted_at IS NULL;
