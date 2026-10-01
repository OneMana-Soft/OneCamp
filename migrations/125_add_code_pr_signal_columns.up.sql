-- Migration 125: promote the code-PR quality signals to columns.
--
-- The reliability scorecard (business/CodePR/metrics.go) aggregates over many
-- runs; reading in_scope / draft out of the judge_verdict JSON per row is both
-- slow and, for `draft`, impossible (it was never persisted — the recorder
-- dropped out.Draft). Promote both to first-class columns so the scorecard is a
-- cheap indexed scan and the admin reliability view is honest.
--
--   in_scope: the scope judge did NOT flag drift (defaults true = fail-open,
--             matching the judge's own fail-open posture).
--   draft:    the PR was opened as a draft (couldn't fully verify).
--
-- Existing rows take the defaults, which is the neutral reading for runs
-- recorded before capture. Idempotent.

ALTER TABLE code_pr_runs
    ADD COLUMN IF NOT EXISTS in_scope boolean NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS draft    boolean NOT NULL DEFAULT false;
