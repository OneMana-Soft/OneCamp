-- Migration 67: per-scope AI memory exclusions (trust & control).
--
-- Lets a channel/project/DM-group be opted OUT of the AI memory layer so
-- nothing from it is ever extracted, captured, or surfaced. This is the
-- privacy-control knob a self-hosting, security-conscious buyer expects:
-- "this sensitive channel is off-limits to the AI."
--
-- A single row per excluded scope. scope_type ∈ channel|project|chat_grp;
-- scope_id is the channel/project uuid or the chat grouping id. Presence of
-- a row == excluded. Removing the row re-enables (future content only;
-- nothing is retroactively re-extracted).

CREATE TABLE IF NOT EXISTS ai_memory_exclusions (
    "scope_type"   varchar NOT NULL
        CHECK (scope_type IN ('channel','project','chat_grp')),
    "scope_id"     varchar NOT NULL,
    "excluded_by"  uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"   TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    PRIMARY KEY (scope_type, scope_id)
);
