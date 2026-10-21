DROP INDEX IF EXISTS idx_guest_grants_slug;
ALTER TABLE guest_grants DROP CONSTRAINT IF EXISTS guest_grants_slug_shape;
ALTER TABLE guest_grants DROP COLUMN IF EXISTS published_by;
ALTER TABLE guest_grants DROP COLUMN IF EXISTS published_at;
ALTER TABLE guest_grants DROP COLUMN IF EXISTS unlock_version;
ALTER TABLE guest_grants DROP COLUMN IF EXISTS password_hash;
ALTER TABLE guest_grants DROP COLUMN IF EXISTS allow_copies;
ALTER TABLE guest_grants DROP COLUMN IF EXISTS indexable;
ALTER TABLE guest_grants DROP COLUMN IF EXISTS slug;
