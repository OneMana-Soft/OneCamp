-- Back to 597's one review per task, pull request and reviewer. This is a
-- schema rollback of data GitHub can re-sync (a task's Refresh reads every
-- review of its pull request again), not an app delete: each reviewer keeps
-- the review their reviews fold to (domain foldReviews, as the read before
-- this rollback took it), and only their other rows go, as 597's key holds
-- one. The fold, for this rollback only: a reviewer's newest review stands,
-- unless it is a reply after an approval or a request for changes, which
-- then stands (ties in the order of review ids, a review with none first).
WITH decided AS (
	SELECT DISTINCT ON (task_id, pr_url, github_login) task_id, pr_url, github_login, id, review_state
	FROM github_pr_reviews WHERE review_state <> 'COMMENTED'
	ORDER BY task_id, pr_url, github_login, submitted_at DESC, review_id DESC NULLS LAST
), newest AS (
	SELECT DISTINCT ON (task_id, pr_url, github_login) task_id, pr_url, github_login, id
	FROM github_pr_reviews
	ORDER BY task_id, pr_url, github_login, submitted_at DESC, review_id DESC NULLS LAST
)
DELETE FROM github_pr_reviews r WHERE r.id NOT IN (
	SELECT CASE WHEN d.review_state IN ('APPROVED', 'CHANGES_REQUESTED') THEN d.id ELSE n.id END
	FROM newest n LEFT JOIN decided d USING (task_id, pr_url, github_login)
);
DROP INDEX IF EXISTS github_pr_reviews_task_pr_login_key;
DROP INDEX IF EXISTS github_pr_reviews_task_pr_review_key;
ALTER TABLE github_pr_reviews DROP COLUMN IF EXISTS review_id;
ALTER TABLE github_pr_reviews DROP CONSTRAINT IF EXISTS github_pr_reviews_task_pr_login_key;
ALTER TABLE github_pr_reviews ADD CONSTRAINT github_pr_reviews_task_pr_login_key UNIQUE (task_id, pr_url, github_login);
