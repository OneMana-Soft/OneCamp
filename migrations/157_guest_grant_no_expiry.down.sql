-- Non-expiring grants cannot be represented once the column is NOT NULL again.
-- They are given the old ceiling rather than being deleted: revoking somebody's
-- working link is a bigger surprise on a rollback than shortening it.
UPDATE guest_grants SET expires_at = NOW() + INTERVAL '90 days' WHERE expires_at IS NULL;
ALTER TABLE guest_grants ALTER COLUMN expires_at SET NOT NULL;
