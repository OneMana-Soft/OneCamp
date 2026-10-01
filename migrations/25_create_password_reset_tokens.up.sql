CREATE TABLE IF NOT EXISTS password_reset_tokens (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "user_id" uuid NOT NULL REFERENCES users(id),
    "token" varchar NOT NULL UNIQUE,
    "expires_at" TIMESTAMP WITH TIME ZONE NOT NULL,
    "used" boolean NOT NULL DEFAULT false,
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_password_reset_tokens_token ON password_reset_tokens(token) WHERE used = false;
