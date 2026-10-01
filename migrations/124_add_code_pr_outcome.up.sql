-- Migration 124: agent code-PR outcome capture (the learning-loop ground truth).
--
-- Adds the terminal outcome of each agent-opened PR to the code_pr_runs ledger
-- (migration 123) so the quality scorecard (business/CodePR/metrics.go) can
-- compute a real MERGE RATE — the signal a static cloud agent can't learn from.
-- The GitHub PR webhook sets it best-effort by matching pr_url when a PR is
-- merged or closed (see business layer).
--
--   outcome:    '' (unknown / still open) | 'merged' | 'merged_with_edits' |
--               'closed_unmerged'  (kept aligned with codepr.PROutcome)
--   outcome_at: when the terminal state was observed (NULL until then)
--
-- OFF-path safe: existing rows default to '' (unknown), which the scorecard
-- excludes from the merge-rate denominator, so nothing is skewed before capture
-- starts. Idempotent.

ALTER TABLE code_pr_runs
    ADD COLUMN IF NOT EXISTS outcome    text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS outcome_at timestamptz;

-- The webhook matches an incoming closed/merged PR to its run by pr_url; index
-- only the rows that actually opened a PR (pr_url present) so the lookup is a
-- cheap partial-index probe regardless of ledger size.
CREATE INDEX IF NOT EXISTS idx_code_pr_runs_pr_url
    ON code_pr_runs (pr_url)
    WHERE pr_url <> '';
