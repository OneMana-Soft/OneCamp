-- Each review is a row of its own, by GitHub's review id (V-1060), and a
-- task's read folds a reviewer's reviews in the order they were submitted
-- (domain foldReviews), so they read the same whatever order they arrive in.
-- A dismissal marks the review it names DISMISSED, kept: every delete is soft.
-- Rows written before this carry no id: each is a reviewer's folded decision,
-- and keeps 597's key, one per task, pull request and reviewer, as does a
-- review written with no id (the demo's landed change). No index holds user
-- text; task_id stays indexed in full (migration 42) for its foreign key.
ALTER TABLE github_pr_reviews ADD COLUMN IF NOT EXISTS review_id bigint;
CREATE UNIQUE INDEX IF NOT EXISTS github_pr_reviews_task_pr_review_key
	ON github_pr_reviews (task_id, pr_url, review_id) WHERE review_id IS NOT NULL;
ALTER TABLE github_pr_reviews DROP CONSTRAINT IF EXISTS github_pr_reviews_task_pr_login_key;
CREATE UNIQUE INDEX IF NOT EXISTS github_pr_reviews_task_pr_login_key
	ON github_pr_reviews (task_id, pr_url, github_login) WHERE review_id IS NULL;
