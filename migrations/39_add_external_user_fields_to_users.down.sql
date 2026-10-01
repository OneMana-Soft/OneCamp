DROP INDEX IF EXISTS idx_users_github_login;

ALTER TABLE users
DROP COLUMN IF EXISTS is_external,
DROP COLUMN IF EXISTS github_login,
DROP COLUMN IF EXISTS github_avatar_url,
DROP COLUMN IF EXISTS github_html_url,
DROP COLUMN IF EXISTS display_name;
