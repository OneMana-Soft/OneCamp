-- Migration 182: passkeys. A person signs in with a passkey (WebAuthn) kept
-- on their device or in their password manager. The credential is stored as
-- the library keeps it (public key, sign count, flags); nothing secret is.
CREATE TABLE IF NOT EXISTS user_passkeys (
    "id"            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "user_id"       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "credential_id" bytea NOT NULL UNIQUE,
    "credential"    jsonb NOT NULL,
    "name"          varchar(60) NOT NULL,
    "created_at"    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "last_used_at"  TIMESTAMP WITH TIME ZONE
);
CREATE INDEX IF NOT EXISTS user_passkeys_user ON user_passkeys (user_id);
