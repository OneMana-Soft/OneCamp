-- Two-factor authentication: TOTP (RFC 6238) plus single-use recovery codes.
--
-- Added because its absence was a hard gate rather than a missing nicety. "Do you support MFA" is the
-- first question on every security questionnaire, and an answer of no ends the conversation regardless
-- of what else the product does — which for this workspace included per-object live authorization,
-- SAML, OIDC and a full agent audit trail. The capability that unlocked those conversations was the
-- one that was missing.
--
-- WHY A SEPARATE KEK. totp_secret_enc is sealed with TOTP_KEK, not with AI_CONFIG_KEK. The temptation
-- is to reuse the existing key rather than ask an operator for another, and it is the wrong call here:
-- AI_CONFIG_KEK is rotated when a provider key leaks, and this deployment has already had it replaced
-- once. Rotating a key that also seals TOTP secrets would lock every enrolled user out of their own
-- account as a side effect of an unrelated credential change. Independent blast radius is the same
-- reasoning that already keeps AI_CONFIG_KEK and IMPORT_TOKEN_KEK apart.
--
-- Enrolment REFUSES when TOTP_KEK is unset rather than falling back to a weaker key. An operator who
-- never sets it simply cannot turn 2FA on, which is a visible dead end. The alternative — sealing
-- secrets under a default — produces enrolled users whose second factor protects nothing, and nobody
-- finds out.

ALTER TABLE users ADD COLUMN IF NOT EXISTS "totp_secret_enc" bytea;

-- NULL means "a secret exists but the user never proved they could read it".
--
-- Enrolment is two steps on purpose: generating a secret is not enrolling. A user who scans a QR code,
-- closes the tab, and never enters a code must NOT be locked out at their next login. Only a confirmed
-- row challenges, so an abandoned enrolment is inert.
ALTER TABLE users ADD COLUMN IF NOT EXISTS "totp_confirmed_at" TIMESTAMP WITH TIME ZONE;

-- The last time-step this user authenticated with, so a code cannot be spent twice.
--
-- A TOTP code is valid for its window plus the skew either side — up to 90 seconds here. Without this,
-- a code read over a shoulder or out of a screen share is usable again for the rest of that window.
-- Storing the step and refusing anything at or below it is what closes that, and it is why
-- helpers.ValidateTOTP returns the matched step instead of a boolean.
--
-- 0 is the default and means "nothing spent yet", which is correct for every existing row: no user is
-- enrolled at the moment this migration runs.
ALTER TABLE users ADD COLUMN IF NOT EXISTS "totp_last_step" bigint NOT NULL DEFAULT 0;

-- Recovery codes: the answer to a lost or wiped phone.
--
-- Without them, losing a device means an administrator has to disable 2FA on an account out of band,
-- which is both a support burden and a social-engineering target — "I lost my phone, please turn off my
-- second factor" is the easiest pretext there is. Self-service recovery with codes the user stored at
-- enrolment removes the need for that conversation.
CREATE TABLE IF NOT EXISTS user_recovery_codes (
    "id"         uuid PRIMARY KEY NOT NULL DEFAULT (uuid_generate_v4()),
    "user_id"    uuid NOT NULL REFERENCES users ("id") ON DELETE CASCADE,

    -- SHA-256, NOT bcrypt, and that is a considered difference from password_hash.
    --
    -- bcrypt exists to make guessing a LOW-ENTROPY human secret expensive. A recovery code is
    -- generated, not chosen, and carries far more entropy than any password a person invents, so
    -- stretching buys nothing against a brute force that is already infeasible. It costs something
    -- real: verification has to find which of a user's codes was presented, and with bcrypt that
    -- means one expensive comparison PER STORED CODE — turning a login into roughly a second of CPU
    -- and handing anyone an unauthenticated way to burn it. A single fast hash with an index answers
    -- the same question in one indexed lookup.
    "code_hash"  varchar NOT NULL,

    -- Set when spent. The row is KEPT rather than deleted so "you have three codes left, and one was
    -- used on the 4th" is answerable; a deleted row cannot tell anyone that a code was ever redeemed,
    -- which is exactly the event worth noticing if it was not you.
    "used_at"    TIMESTAMP WITH TIME ZONE,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- Verification looks a code up for one user, so the index is on the pair.
--
-- UNIQUE per user, not globally: two users legitimately holding the same generated code is astronomically
-- unlikely but not impossible, and a global constraint would turn that coincidence into a failed
-- enrolment for the second user. Scoped per user it still stops a code being stored twice for the same
-- person, which is the case that would let one code be redeemed more than once.
CREATE UNIQUE INDEX IF NOT EXISTS "user_recovery_codes_user_hash_idx"
    ON user_recovery_codes ("user_id", "code_hash");

-- Listing a user's remaining codes, and cascading a delete.
CREATE INDEX IF NOT EXISTS "user_recovery_codes_user_idx"
    ON user_recovery_codes ("user_id");
