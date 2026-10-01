-- Irreversible by design: the plaintext tokens are gone and cannot be derived from
-- their hashes, which is the entire point of the change. Rolling the application
-- back is enough — it will write and read plaintext again — and any token issued
-- while the new code ran simply will not validate against the old code. They expire
-- within the hour.
DELETE FROM password_reset_tokens WHERE used = false;
