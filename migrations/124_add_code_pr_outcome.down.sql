-- Rollback migration 124: drop the code-PR outcome-capture column + index.
DROP INDEX IF EXISTS idx_code_pr_runs_pr_url;

ALTER TABLE code_pr_runs
    DROP COLUMN IF EXISTS outcome_at,
    DROP COLUMN IF EXISTS outcome;
