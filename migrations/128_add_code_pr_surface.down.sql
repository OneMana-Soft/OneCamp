DROP INDEX IF EXISTS idx_code_pr_runs_pr_url;

ALTER TABLE code_pr_runs
    DROP COLUMN IF EXISTS surface;
