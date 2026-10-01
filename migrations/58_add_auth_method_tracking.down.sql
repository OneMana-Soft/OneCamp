DROP INDEX IF EXISTS idx_users_signup_method;
ALTER TABLE users DROP COLUMN IF EXISTS "is_sso_managed";
ALTER TABLE users DROP COLUMN IF EXISTS "last_login_at";
ALTER TABLE users DROP COLUMN IF EXISTS "last_login_method";
ALTER TABLE users DROP COLUMN IF EXISTS "signup_method";
