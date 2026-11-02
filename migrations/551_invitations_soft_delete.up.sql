-- V-184 head 3c (the owner's rule: every delete is soft): an admin
-- withdrawing an invitation marks its rows, when and by whom, instead of
-- removing them, so who invited whom, and who withdrew it and when, stay on
-- record. The withdrawal also clears the link (token_hash, token_sealed and
-- the old clear token): it is the invited person's credential and must not
-- outlive the withdrawal. Every read of invitations takes live rows only
-- (models/postgres/SoftDelete.Live). Inviting the address again makes a new
-- row, as it did after a withdrawal before, and the withdrawn one stays.
ALTER TABLE invitations ADD COLUMN IF NOT EXISTS "deleted_at" TIMESTAMP WITH TIME ZONE, ADD COLUMN IF NOT EXISTS "deleted_by" uuid;
