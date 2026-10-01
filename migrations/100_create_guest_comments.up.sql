-- Migration 100: Guest comments on shared docs.
--
-- A guest holding a doc grant with capability = 'comment' may post comments on
-- that one doc. These are stored in an ISOLATED table, deliberately decoupled
-- from the core `comments` table (which has created_by -> users(id)). A guest
-- has no users row, so guest comments must never touch that FK, the Dgraph
-- comment_by User node, OpenSearch, AI embeddings, or member notifications.
--
-- Attribution is to the badged guest identity only (the grant + the sanitized
-- display name captured at post time). A guest comment can NEVER resolve to a
-- member profile.
--
-- Body is stored as PLAIN TEXT (HTML stripped at write time): guests are
-- untrusted external users and their comments render inside authenticated
-- member sessions, so storing/echoing raw HTML would be a stored-XSS vector.

CREATE TABLE IF NOT EXISTS guest_comments (
    "id"           uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    -- The grant that authorized this comment. ON DELETE CASCADE so revoking a
    -- grant row (if ever hard-deleted) cleans up; normal revoke is soft.
    "grant_id"     uuid NOT NULL REFERENCES guest_grants(id) ON DELETE CASCADE,
    -- The doc uuid this comment belongs to (denormalized from the grant for a
    -- fast per-doc listing path and so the doc id is fixed at write time).
    "doc_uuid"     varchar NOT NULL,
    -- Sanitized guest display name captured at post time (point-in-time; the
    -- grant carries no name). Never a member identity.
    "guest_name"   varchar NOT NULL,
    -- Plain-text comment body (HTML already stripped). Length-capped at write.
    "body"         text NOT NULL,
    "created_at"   timestamptz NOT NULL DEFAULT now(),
    "deleted_at"   timestamptz
);

-- Primary read path: all live (not deleted) comments for one doc, oldest first.
CREATE INDEX IF NOT EXISTS idx_guest_comments_doc
    ON guest_comments (doc_uuid, created_at)
    WHERE deleted_at IS NULL;

-- Cleanup path when a grant is revoked/expired.
CREATE INDEX IF NOT EXISTS idx_guest_comments_grant ON guest_comments (grant_id);
