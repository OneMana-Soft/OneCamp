-- Migration 74: Admin configuration audit log.
--
-- Enterprise/compliance requirement (SOC2/ISO): a tamper-evident record of who
-- changed which sensitive workspace setting, when, and from where. Now that
-- admins can configure secrets in-app (GitHub App creds, OAuth creds, Resend
-- key, app signing secrets, upload limit, allow-list), every such change is
-- recorded here.
--
-- Secret VALUES are never stored — only the fact that a field changed, plus
-- redacted before/after summaries (e.g. "set"/"cleared", or a non-secret value
-- like the upload limit). This keeps the audit useful without becoming a second
-- place secrets can leak from.

CREATE TABLE IF NOT EXISTS admin_audit_log (
    "id"          uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    -- Actor: the admin who made the change.
    "actor_id"    uuid REFERENCES users(id),
    "actor_email" varchar,
    -- What was changed. action is a stable machine key
    -- (e.g. "settings.upload_limit", "github.client_secret", "oauth.google").
    "action"      varchar NOT NULL,
    -- Human-readable category for filtering in the UI.
    "category"    varchar NOT NULL,           -- settings | integration | auth | app | security
    -- Redacted change summary (NEVER raw secret values).
    "summary"     text NOT NULL,
    -- Optional structured detail (redacted), e.g. {"field":"upload_limit_mb","from":"10","to":"50"}.
    "metadata"    jsonb,
    -- Request provenance.
    "ip_address"  varchar,
    "user_agent"  varchar,
    "created_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Newest-first listing is the dominant query; index created_at desc.
CREATE INDEX IF NOT EXISTS idx_admin_audit_log_created_at ON admin_audit_log (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_log_category ON admin_audit_log (category, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_log_actor ON admin_audit_log (actor_id, created_at DESC);
