-- Policy values set for one thing beside the workspace (business/Policy's
-- scoped policies): a project's own "Who can change tasks", say. A value here
-- beats the workspace's (system_configs) for its scope; with none, the
-- workspace's value applies, then the policy's default.
CREATE TABLE IF NOT EXISTS policy_scoped_values (
    scope_kind varchar(32)  NOT NULL,
    scope_id   uuid         NOT NULL,
    key        varchar(128) NOT NULL,
    value      text         NOT NULL,
    set_by     uuid         NULL,
    set_at     timestamptz  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (scope_kind, scope_id, key)
);
