ALTER TABLE users
ADD COLUMN IF NOT EXISTS is_external boolean NOT NULL DEFAULT false,
ADD COLUMN IF NOT EXISTS github_login text,
ADD COLUMN IF NOT EXISTS github_avatar_url text,
ADD COLUMN IF NOT EXISTS github_html_url text,
ADD COLUMN IF NOT EXISTS display_name text;

CREATE INDEX IF NOT EXISTS idx_users_github_login ON users(github_login) WHERE github_login IS NOT NULL;
