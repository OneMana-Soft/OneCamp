-- Indexes another index already serves, on small tables, so a plain DROP in
-- one transaction (none of these tables grows per message or event):
--   poll_votes_poll_idx                        poll_votes_pkey (poll_id, user_id, option_id)
--   idx_archive_job_items_job                  archive_job_items_pkey (job_id, entity_id)
--   idx_users_notification_preferences_token   UNIQUE (unsubscribe_token)
--   idx_notification_email_suppressions_email  UNIQUE (email)
--   user_recovery_codes_user_idx               user_recovery_codes_user_hash_idx (user_id, code_hash)
--   idx_github_links_project_id                idx_github_links_project_repo (project_id, ...) WHERE deleted_at IS NULL
--   idx_github_reaction_refs_task, _lookup     UNIQUE (task_id, user_id, github_name); only where 52
--                                              kept a non-empty github_reaction_refs_legacy_38
-- And indexes no query reads:
--   idx_webhooks_token       a webhook is found by token_hash (UNIQUE, 490); UNIQUE (token) stays
--   idx_users_signup_method  signup_method is written, never searched
--   idx_users_github_login   the one lookup asks LOWER(github_login) = LOWER($1), which a key
--                            on github_login can't serve: idx_users_github_login_lower does
-- The big tables' redundant indexes drop CONCURRENTLY, each in a file of its own.

DROP INDEX IF EXISTS poll_votes_poll_idx;
DROP INDEX IF EXISTS idx_archive_job_items_job;
DROP INDEX IF EXISTS idx_users_notification_preferences_token;
DROP INDEX IF EXISTS idx_notification_email_suppressions_email;
DROP INDEX IF EXISTS user_recovery_codes_user_idx;
DROP INDEX IF EXISTS idx_github_links_project_id;
DROP INDEX IF EXISTS idx_github_reaction_refs_task;
DROP INDEX IF EXISTS idx_github_reaction_refs_lookup;
DROP INDEX IF EXISTS idx_webhooks_token;
DROP INDEX IF EXISTS idx_users_signup_method;
DROP INDEX IF EXISTS idx_users_github_login;

CREATE INDEX IF NOT EXISTS idx_users_github_login_lower ON users (lower(github_login));

-- webhook_commands (46) was never written: the one command registry is
-- slash_commands (73). It goes only while it is empty; a table with rows stays.
DO $$
DECLARE
    has_rows boolean;
BEGIN
    IF to_regclass('webhook_commands') IS NOT NULL THEN
        EXECUTE 'SELECT EXISTS (SELECT 1 FROM webhook_commands)' INTO has_rows;
        IF NOT has_rows THEN
            DROP TABLE webhook_commands;
        END IF;
    END IF;
END $$;
