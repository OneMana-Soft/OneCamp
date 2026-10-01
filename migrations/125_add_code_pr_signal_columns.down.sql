-- Rollback migration 125: drop the promoted code-PR signal columns.
ALTER TABLE code_pr_runs
    DROP COLUMN IF EXISTS draft,
    DROP COLUMN IF EXISTS in_scope;
