-- Track how each user signed up and most-recently authenticated, and lock SSO
-- users out of local-password backdoors.
--
-- signup_method   one of: email | google | github | oidc | saml | ldap | demo
-- last_login_method  same set, updated on each successful login
-- is_sso_managed  true when the user must authenticate via an external IdP
--                 (LDAP/SAML/OIDC). When true, SetPassword/ChangePassword
--                 are rejected by the API to prevent local backdoors.
ALTER TABLE users ADD COLUMN IF NOT EXISTS "signup_method" varchar;
ALTER TABLE users ADD COLUMN IF NOT EXISTS "last_login_method" varchar;
ALTER TABLE users ADD COLUMN IF NOT EXISTS "last_login_at" TIMESTAMP WITH TIME ZONE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS "is_sso_managed" boolean NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS idx_users_signup_method ON users(signup_method);
