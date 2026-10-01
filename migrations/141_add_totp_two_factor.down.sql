-- Reverses migration 141.
--
-- Dropping the table takes every enrolled user's recovery codes with it, and dropping the columns takes
-- their enrolment. That is the correct behaviour for a down-migration — it must leave the schema as it
-- was — but it is worth stating plainly: rolling this back and then rolling forward again does not
-- restore anyone's second factor. Every previously-enrolled user has to enrol afresh, and any recovery
-- code they wrote down is gone.

DROP INDEX IF EXISTS "user_recovery_codes_user_idx";
DROP INDEX IF EXISTS "user_recovery_codes_user_hash_idx";
DROP TABLE IF EXISTS user_recovery_codes;

ALTER TABLE users DROP COLUMN IF EXISTS "totp_last_step";
ALTER TABLE users DROP COLUMN IF EXISTS "totp_confirmed_at";
ALTER TABLE users DROP COLUMN IF EXISTS "totp_secret_enc";
