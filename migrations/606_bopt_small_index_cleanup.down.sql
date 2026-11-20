-- Each index and table as the migration that made it wrote it.

-- 173_create_polls
CREATE INDEX IF NOT EXISTS poll_votes_poll_idx ON poll_votes (poll_id);
-- 63_archive_job_items
CREATE INDEX IF NOT EXISTS idx_archive_job_items_job
    ON archive_job_items(job_id);
-- 59_create_email_notifications
CREATE INDEX IF NOT EXISTS idx_users_notification_preferences_token
    ON users_notification_preferences(unsubscribe_token);
CREATE INDEX IF NOT EXISTS idx_notification_email_suppressions_email
    ON notification_email_suppressions(email);
-- 141_add_totp_two_factor
CREATE INDEX IF NOT EXISTS "user_recovery_codes_user_idx"
    ON user_recovery_codes ("user_id");
-- 28_create_github_links_table
CREATE INDEX IF NOT EXISTS idx_github_links_project_id ON github_links(project_id) WHERE deleted_at IS NULL;
-- 26_create_webhooks_table
CREATE INDEX IF NOT EXISTS idx_webhooks_token ON webhooks(token) WHERE deleted_at IS NULL;
-- 58_add_auth_method_tracking
CREATE INDEX IF NOT EXISTS idx_users_signup_method ON users(signup_method);
-- 39_add_external_user_fields_to_users (and 43)
CREATE INDEX IF NOT EXISTS idx_users_github_login ON users(github_login) WHERE github_login IS NOT NULL;

DROP INDEX IF EXISTS idx_users_github_login_lower;

-- 38_create_github_reaction_refs, on the table 52 renamed when it held rows
DO $$
BEGIN
    IF to_regclass('github_reaction_refs_legacy_38') IS NOT NULL THEN
        CREATE INDEX IF NOT EXISTS idx_github_reaction_refs_task ON github_reaction_refs_legacy_38(task_id);
        CREATE INDEX IF NOT EXISTS idx_github_reaction_refs_lookup ON github_reaction_refs_legacy_38(task_id, user_id, github_name);
    END IF;
END $$;

-- 46_webhook_production_hardening (a no-op where the up kept it)
CREATE TABLE IF NOT EXISTS webhook_commands (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "webhook_id" uuid NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    "command" varchar NOT NULL,
    "description" text,
    "handler_url" text,          -- Optional: POST to this URL instead of inline processing
    "response_type" varchar DEFAULT 'in_channel', -- ephemeral or in_channel
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE,
    UNIQUE(webhook_id, command)
);

CREATE INDEX IF NOT EXISTS idx_webhook_commands_webhook_id ON webhook_commands(webhook_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_webhook_commands_command ON webhook_commands(command) WHERE deleted_at IS NULL;
