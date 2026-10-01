ALTER TABLE users ADD COLUMN IF NOT EXISTS "password_hash" varchar;
ALTER TABLE users ADD COLUMN IF NOT EXISTS "username" varchar UNIQUE;
