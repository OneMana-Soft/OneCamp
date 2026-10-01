-- Store PR review state per task
CREATE TABLE IF NOT EXISTS github_pr_reviews (
    id uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    task_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    github_login text NOT NULL,
    github_avatar_url text,
    github_html_url text,
    review_state varchar NOT NULL CHECK (review_state IN ('APPROVED', 'CHANGES_REQUESTED', 'COMMENTED')),
    submitted_at timestamptz NOT NULL DEFAULT NOW(),
    UNIQUE (task_id, github_login)
);

CREATE INDEX IF NOT EXISTS idx_github_pr_reviews_task_id ON github_pr_reviews(task_id);
CREATE INDEX IF NOT EXISTS idx_github_pr_reviews_submitted_at ON github_pr_reviews(submitted_at);
