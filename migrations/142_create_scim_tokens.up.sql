-- SCIM provisioning credentials.
--
-- A SEPARATE TABLE FROM api_tokens, and the reason is a circular outage rather than tidiness.
-- api_tokens.created_by is a user, and middleware/apiToken.go refuses any request whose token owner is
-- not an ACTIVE user. SCIM's whole job is deactivating users. So the moment the directory offboards the
-- administrator who connected it -- which is exactly what happens when the IT person who set up Okta
-- leaves -- every subsequent provisioning call starts answering 401, silently, and new joiners simply
-- stop getting accounts. A credential must not be revocable by the process it authorises.
--
-- So this credential belongs to the WORKSPACE, not to a person. created_by is recorded for audit only
-- and is ON DELETE SET NULL: who set the integration up is worth knowing, and it must never be able to
-- take the integration down with them.
CREATE TABLE IF NOT EXISTS scim_tokens (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT (uuid_generate_v4()),
    -- Operator-facing label, e.g. "Okta production". Shown in admin settings so a credential can be
    -- revoked without having to guess which IdP it belongs to.
    "name" varchar NOT NULL,
    -- SHA-256 hex of the secret. The secret itself is shown once at creation and never stored, the same
    -- way api_tokens works. Unsalted is correct here and not an oversight: the input is 160 bits of CSPRNG
    -- output, not a human-chosen password, so there is no dictionary for a rainbow table to be built from
    -- and the lookup has to be by exact hash to be a single indexed read.
    "token_hash" varchar NOT NULL,
    -- First few characters of the secret, kept so the UI can show WHICH credential a row is without
    -- being able to reconstruct it.
    "token_prefix" varchar NOT NULL,
    -- Audit only. SET NULL rather than CASCADE: see the note above.
    "created_by" uuid REFERENCES users(id) ON DELETE SET NULL,
    "last_used_at" TIMESTAMP WITH TIME ZONE,
    "expires_at" TIMESTAMP WITH TIME ZONE,
    "revoked_at" TIMESTAMP WITH TIME ZONE,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- Every authenticated SCIM request is one lookup by this value, so it carries the uniqueness constraint
-- as well as the index: two rows with one hash would make "which credential was used" unanswerable.
CREATE UNIQUE INDEX IF NOT EXISTS scim_tokens_token_hash_key ON scim_tokens ("token_hash");
