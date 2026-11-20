-- A reviewer's review stands per pull request (V-986): a task's next pull
-- request starts with no review, and a late review on the first one stays
-- the first one's. Rows written before this belong to the pull request their
-- task has now (an address over 512 characters is no GitHub pull request's,
-- and would outgrow the key's index entry: its reviews stay at '', unread). A
-- dismissed review is kept, as DISMISSED, and counts for
-- nothing: GitHub's review decision drops it.
ALTER TABLE github_pr_reviews ADD COLUMN IF NOT EXISTS pr_url text NOT NULL DEFAULT '';
UPDATE github_pr_reviews r SET pr_url = t.github_pr_url
	FROM tasks t WHERE t.id = r.task_id AND t.github_pr_url IS NOT NULL AND r.pr_url = ''
		AND length(t.github_pr_url) <= 512;
ALTER TABLE github_pr_reviews DROP CONSTRAINT IF EXISTS github_pr_reviews_task_id_github_login_key;
ALTER TABLE github_pr_reviews DROP CONSTRAINT IF EXISTS github_pr_reviews_task_pr_login_key;
ALTER TABLE github_pr_reviews ADD CONSTRAINT github_pr_reviews_task_pr_login_key UNIQUE (task_id, pr_url, github_login);
ALTER TABLE github_pr_reviews DROP CONSTRAINT IF EXISTS github_pr_reviews_review_state_check;
ALTER TABLE github_pr_reviews ADD CONSTRAINT github_pr_reviews_review_state_check
	CHECK (review_state IN ('APPROVED', 'CHANGES_REQUESTED', 'COMMENTED', 'DISMISSED'));
