-- Migration 97: Guest access grants.
--
-- A guest grant is a scoped, expiring authorization for an EXTERNAL person
-- (no OneCamp account) to reach exactly ONE resource for a bounded time:
--   - Phase 1: a meeting room (resource_type = 'meeting', capability = 'join')
--   - Phase 2: a doc/board/table (resource_type in 'doc'|'board'|'table',
--              capability in 'view'|'comment')
--
-- Security posture (mirrors api_tokens / webhook secrets):
--   - The raw link token is shown ONCE in the share URL; only its SHA-256 is
--     stored here. A leaked DB never yields a usable token.
--   - A grant is USABLE iff: row found by token_hash AND revoked_at IS NULL AND
--     expires_at > now() AND the workspace guest-access policy is ON.
--   - Guests are NEVER written to the users table, so they cannot leak into
--     rosters, search, memory, mentions, or nudges by construction.

CREATE TABLE IF NOT EXISTS guest_grants (
    "id"            uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    -- SHA-256 of the opaque URL-safe link token. Never store the raw token.
    "token_hash"    bytea NOT NULL,
    -- 'meeting' (Phase 1); 'doc' | 'board' | 'table' (Phase 2).
    "resource_type" varchar NOT NULL,
    -- Room name for a meeting; resource uuid for doc/board/table.
    "resource_id"   varchar NOT NULL,
    -- 'join' (meeting) | 'view' | 'comment' (resource).
    "capability"    varchar NOT NULL,
    -- The member who created the grant.
    "created_by"    uuid NOT NULL REFERENCES users(id),
    -- Absolute expiry; enforced on every guest request.
    "expires_at"    timestamptz NOT NULL,
    -- Set when an admin/owner revokes; NULL = active.
    "revoked_at"    timestamptz,
    "created_at"    timestamptz NOT NULL DEFAULT now()
);

-- Primary lookup path: validate an incoming link by its token hash.
CREATE UNIQUE INDEX IF NOT EXISTS idx_guest_grants_token_hash ON guest_grants (token_hash);

-- Admin "active grants for this resource" listing.
CREATE INDEX IF NOT EXISTS idx_guest_grants_active
    ON guest_grants (resource_type, resource_id)
    WHERE revoked_at IS NULL;

-- Newest-first admin listing.
CREATE INDEX IF NOT EXISTS idx_guest_grants_created_at ON guest_grants (created_at DESC);
