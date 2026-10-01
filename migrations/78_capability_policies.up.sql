-- Migration 78: Generic capability-permission policies.
--
-- A reusable authorization layer that lets workspace admins delegate specific
-- capabilities to all members (Slack's "Permissions" model), instead of every
-- capability being hard-wired to admin-only.
--
-- Why generic (not a per-feature boolean): capabilities like "create
-- workflows", "invite members", "manage apps" share one shape — a setting that
-- is either admins_only or all_members. Modeling them as rows in one table
-- means adding a new delegatable capability is a one-line constant in code, not
-- a schema migration, and the FE/permission middleware stay uniform.
--
-- Capabilities NOT in this table are implicitly admins_only (fail-closed): the
-- permission helper treats an unknown/missing capability as admin-only, so a
-- governance-sensitive action can never be accidentally opened up.

CREATE TABLE IF NOT EXISTS capability_policies (
    -- The capability key, e.g. 'workflow.manage', 'invitation.create'. Stable
    -- string referenced from code constants.
    "capability"  varchar PRIMARY KEY,

    -- Who may exercise it: 'admins_only' (default) or 'all_members'.
    "policy"      varchar NOT NULL DEFAULT 'admins_only'
        CHECK (policy IN ('admins_only', 'all_members')),

    -- Audit: which admin last changed this, and when.
    "updated_by"  uuid REFERENCES users(id) ON DELETE SET NULL,
    "updated_at"  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- Seed the delegatable capabilities at their safe defaults (admins_only). An
-- admin opts a capability open to all members via the Settings UI. Rows are
-- seeded so the Settings UI can render the full toggle list without a code
-- catalog round-trip, and so a missing row vs. an explicit admins_only are
-- indistinguishable (both fail-closed).
INSERT INTO capability_policies (capability, policy) VALUES
    ('workflow.manage',   'admins_only'),
    ('invitation.create', 'admins_only'),
    ('app.manage',        'admins_only'),
    ('webhook.manage',    'admins_only')
ON CONFLICT (capability) DO NOTHING;
