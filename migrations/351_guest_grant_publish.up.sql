-- A doc's "Anyone with the link" grant can also be published as a web page
-- (docs C6, decision D-25): the same grant, reached by a readable address as
-- well as its token. One external-access model: expiry, revoke and audit stay
-- the grant's own, and the workspace's guest-access switch governs both.
--
--   slug           the page's address, /public/p/<slug>; NULL when the link
--                  isn't published. Unique among grants nobody revoked, so a
--                  revoked link's address can be used again.
--   indexable      "Let search engines find this page"; off unless turned on.
--   allow_copies   members may copy the page into their own docs.
--   password_hash  bcrypt; NULL for none. A grant with a password is reached
--                  only through the server's page, after its unlock.
--   unlock_version moves when the password or the address changes, ending
--                  every unlock given before.
--   published_at / published_by  when and by whom it was published.
ALTER TABLE guest_grants ADD COLUMN IF NOT EXISTS slug text;
ALTER TABLE guest_grants ADD COLUMN IF NOT EXISTS indexable boolean NOT NULL DEFAULT false;
ALTER TABLE guest_grants ADD COLUMN IF NOT EXISTS allow_copies boolean NOT NULL DEFAULT false;
ALTER TABLE guest_grants ADD COLUMN IF NOT EXISTS password_hash text;
ALTER TABLE guest_grants ADD COLUMN IF NOT EXISTS unlock_version integer NOT NULL DEFAULT 1;
ALTER TABLE guest_grants ADD COLUMN IF NOT EXISTS published_at timestamptz;
ALTER TABLE guest_grants ADD COLUMN IF NOT EXISTS published_by uuid;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'guest_grants_slug_shape') THEN
        ALTER TABLE guest_grants ADD CONSTRAINT guest_grants_slug_shape
            CHECK (slug IS NULL OR slug ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$');
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_guest_grants_slug ON guest_grants (slug)
    WHERE slug IS NOT NULL AND revoked_at IS NULL;
