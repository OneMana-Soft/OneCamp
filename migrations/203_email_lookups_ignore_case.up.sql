-- Migration 203: accounts and invitations are found by address without regard
-- to case. Addresses are lowercased as they arrive now (helpers.NormalizeEmail),
-- but accounts made before keep the case they came with, so every lookup
-- compares LOWER(email_id) = LOWER($1), which the unique index on email_id
-- cannot serve. These can.
--
-- Not unique: two accounts whose addresses differ only in case can exist from
-- before this, and choosing between them is a decision for a person, not for a
-- migration.
CREATE INDEX IF NOT EXISTS users_email_id_lower_idx ON users (LOWER(email_id));
CREATE INDEX IF NOT EXISTS invitations_email_lower_idx ON invitations (LOWER(email));
