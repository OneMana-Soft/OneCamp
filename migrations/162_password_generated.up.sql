-- Whether the account still has the password it was handed.
--
-- OneCamp Cloud creates the owner's account with a generated password and
-- emails it once. Nothing asked them to replace it, so the one credential that
-- has sat in an inbox stayed the credential. The setup checklist shows a step
-- while this is true; any password change clears it.
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_generated boolean NOT NULL DEFAULT false;
