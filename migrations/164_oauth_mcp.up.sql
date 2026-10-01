-- OAuth 2.1 for the MCP endpoint (/v1/mcp).
--
-- Claude, Cowork and ChatGPT connect to a remote MCP server by URL and sign the
-- person in with OAuth; most of them cannot send a pasted bearer token. These
-- tables let them. What they end up holding is an ordinary api_tokens row bound
-- to an agent identity, so the inventory lists it, the agent's kill switch
-- stops it and the audit log names it, exactly like a token made by hand.

-- A client that registered itself (RFC 7591 dynamic registration). Public
-- clients only: no secret is issued, PKCE carries the proof instead.
CREATE TABLE IF NOT EXISTS oauth_clients (
    "id"            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "client_name"   varchar NOT NULL,
    "redirect_uris" jsonb NOT NULL,
    "created_at"    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "last_used_at"  TIMESTAMP WITH TIME ZONE
);

-- A sign-in waiting for the person to approve it. Minutes long.
CREATE TABLE IF NOT EXISTS oauth_requests (
    "id"             uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "client_id"      uuid NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    "redirect_uri"   text NOT NULL,
    "state"          text NOT NULL DEFAULT '',
    "scopes"         jsonb NOT NULL,
    "code_challenge" varchar NOT NULL,
    "expires_at"     TIMESTAMP WITH TIME ZONE NOT NULL
);

-- An approved sign-in, exchanged once for tokens. Stored as a hash.
CREATE TABLE IF NOT EXISTS oauth_codes (
    "code_hash"      varchar PRIMARY KEY,
    "client_id"      uuid NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    "user_id"        uuid NOT NULL,
    "agent_id"       uuid NOT NULL,
    "scopes"         jsonb NOT NULL,
    "redirect_uri"   text NOT NULL,
    "code_challenge" varchar NOT NULL,
    "expires_at"     TIMESTAMP WITH TIME ZONE NOT NULL
);

-- A live connection: the credential it renews and the refresh token that
-- renews it (hash only). Revoking the credential ends the connection.
CREATE TABLE IF NOT EXISTS oauth_grants (
    "id"                 uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "client_id"          uuid NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    "user_id"            uuid NOT NULL,
    "agent_id"           uuid NOT NULL,
    "token_id"           uuid NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    "refresh_hash"       varchar NOT NULL UNIQUE,
    "refresh_expires_at" TIMESTAMP WITH TIME ZONE NOT NULL,
    "created_at"         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "revoked_at"         TIMESTAMP WITH TIME ZONE
);
CREATE INDEX IF NOT EXISTS oauth_grants_user_agent_idx ON oauth_grants (user_id, agent_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS oauth_requests_expires_idx ON oauth_requests (expires_at);
CREATE INDEX IF NOT EXISTS oauth_codes_expires_idx ON oauth_codes (expires_at);
