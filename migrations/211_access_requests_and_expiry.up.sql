-- Migration 211: asking for access, access until a date, and admin recovery
-- (docs programme A5, PROGRAM.md §4 #12: the Access Resolver).
--
-- Who may do what with a resource lives with the resource (for a doc, its
-- grant edges in the graph). These tables hold what the graph can't:
--
--   access_requests      someone asks for access to a resource ("Request
--                        access"); its owner, or someone with full access,
--                        approves or declines. One open request per person
--                        and resource: asking again updates it.
--   access_grant_expiry  a grant that ends: a person or a team given a level
--                        "until" a moment. The expiry sweep takes the grant
--                        away once the moment passes, and the row with it.
--                        The grant itself stays in the graph.
--   access_recoveries    an admin took over a resource whose owner left (or
--                        handed it to someone): who, when, from whom, to whom
--                        and why, so the owner can see it was done (it is in
--                        the admin audit log too).
--
-- resource_kind says what the uuid is ("doc"; later channels, events,
-- recordings). Ids are made in Go: Postgres 12 has no gen_random_uuid().
CREATE TABLE IF NOT EXISTS access_requests (
    "id"             uuid PRIMARY KEY,
    "resource_kind"  varchar(32) NOT NULL,
    "resource_uuid"  uuid        NOT NULL,
    "requester_uuid" uuid        NOT NULL,
    "level_wanted"   varchar(16) NOT NULL CHECK (level_wanted IN ('view', 'comment', 'edit', 'full')),
    "note"           text        NOT NULL DEFAULT '' CHECK (char_length(note) <= 500),
    "status"         varchar(16) NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'approved', 'declined', 'withdrawn')),
    "level_granted"  varchar(16) CHECK (level_granted IS NULL OR level_granted IN ('view', 'comment', 'edit', 'full')),
    "decided_by"     uuid,
    "decided_at"     TIMESTAMP WITH TIME ZONE,
    "created_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- One open request per person and resource.
CREATE UNIQUE INDEX IF NOT EXISTS access_requests_one_open
    ON access_requests (resource_kind, resource_uuid, requester_uuid) WHERE status = 'pending';
-- A resource's requests, open ones first, newest first.
CREATE INDEX IF NOT EXISTS access_requests_resource
    ON access_requests (resource_kind, resource_uuid, status, created_at DESC);
-- A person's own requests.
CREATE INDEX IF NOT EXISTS access_requests_requester
    ON access_requests (requester_uuid, created_at DESC);
-- The open requests, newest first, for the people who answer them.
CREATE INDEX IF NOT EXISTS access_requests_open
    ON access_requests (created_at DESC) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS access_grant_expiry (
    "resource_kind"  varchar(32) NOT NULL,
    "resource_uuid"  uuid        NOT NULL,
    "principal_kind" varchar(16) NOT NULL CHECK (principal_kind IN ('user', 'team')),
    "principal_uuid" uuid        NOT NULL,
    "level"          varchar(16) NOT NULL CHECK (level IN ('view', 'comment', 'edit', 'full')),
    "expires_at"     TIMESTAMP WITH TIME ZONE NOT NULL,
    "created_by"     uuid,
    "created_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY ("resource_kind", "resource_uuid", "principal_kind", "principal_uuid")
);

-- The sweep's read: what has ended.
CREATE INDEX IF NOT EXISTS access_grant_expiry_due ON access_grant_expiry (expires_at);

CREATE TABLE IF NOT EXISTS access_recoveries (
    "id"              uuid PRIMARY KEY,
    "resource_kind"   varchar(32) NOT NULL,
    "resource_uuid"   uuid        NOT NULL,
    "admin_uuid"      uuid        NOT NULL,
    "from_owner_uuid" uuid,
    "to_owner_uuid"   uuid        NOT NULL,
    "reason"          text        NOT NULL DEFAULT '' CHECK (char_length(reason) <= 500),
    "created_at"      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- A resource's recoveries, newest first, for its sharing panel.
CREATE INDEX IF NOT EXISTS access_recoveries_resource
    ON access_recoveries (resource_kind, resource_uuid, created_at DESC);
