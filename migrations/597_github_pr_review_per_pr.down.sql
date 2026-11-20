-- Back to one review per task and reviewer. The reviews a task's read leaves
-- out (domain GetGitHubPRReviewsByTaskId: another pull request's, or a
-- dismissed one) go, as the old key can hold only one and the old read would
-- take any review it holds as the task's: what stays is exactly what the task
-- read, one per reviewer by the new key, so the decision reads the same.
-- A task whose address is over 512 characters loses its reviews here too: up
-- leaves them under no address, so its read takes none.
DELETE FROM github_pr_reviews r
	WHERE r.review_state = 'DISMISSED'
		OR r.pr_url <> COALESCE((SELECT t.github_pr_url FROM tasks t WHERE t.id = r.task_id), '');
ALTER TABLE github_pr_reviews DROP CONSTRAINT IF EXISTS github_pr_reviews_review_state_check;
ALTER TABLE github_pr_reviews ADD CONSTRAINT github_pr_reviews_review_state_check
	CHECK (review_state IN ('APPROVED', 'CHANGES_REQUESTED', 'COMMENTED'));
ALTER TABLE github_pr_reviews DROP CONSTRAINT IF EXISTS github_pr_reviews_task_pr_login_key;
ALTER TABLE github_pr_reviews DROP CONSTRAINT IF EXISTS github_pr_reviews_task_id_github_login_key;
ALTER TABLE github_pr_reviews ADD CONSTRAINT github_pr_reviews_task_id_github_login_key UNIQUE (task_id, github_login);
ALTER TABLE github_pr_reviews DROP COLUMN IF EXISTS pr_url;
