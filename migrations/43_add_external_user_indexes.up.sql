-- Add partial indexes for external user queries and GitHub login lookups
CREATE INDEX IF NOT EXISTS idx_users_is_external ON users(is_external) WHERE is_external = true;
CREATE INDEX IF NOT EXISTS idx_users_github_login ON users(github_login) WHERE github_login IS NOT NULL;
