-- Email notification subsystem.
--
-- Four tables wire up an end-to-end email pipeline that complements the
-- existing FCM/MQTT realtime stack. The whole system is feature-flagged
-- behind RESEND_API_KEY at the service layer, so an operator who never sets
-- it gets zero side effects: no rows inserted, no workers running, no
-- behaviour change for existing flows.

-- 1. Per-user master switch + per-event toggles.
--    The default is "everything on" for installs that have email configured;
--    individual users opt down via /user/updateNotificationPreferences.
--    quiet_hours_* let users defer non-urgent mail to a window in their TZ.
--    unsubscribe_token is one-click via the List-Unsubscribe header
--    (RFC 8058) so Gmail/Outlook can offer the unsubscribe affordance.
CREATE TABLE IF NOT EXISTS users_notification_preferences (
    "id"                          uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "user_id"                     uuid NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    "email_enabled"               boolean NOT NULL DEFAULT true,
    "email_mentions"              boolean NOT NULL DEFAULT true,
    "email_dms"                   boolean NOT NULL DEFAULT true,
    "email_task_assigned"         boolean NOT NULL DEFAULT true,
    "email_task_status"           boolean NOT NULL DEFAULT true,
    "email_comments"              boolean NOT NULL DEFAULT true,
    "email_calls"                 boolean NOT NULL DEFAULT true,
    "email_channel_invites"       boolean NOT NULL DEFAULT true,
    "email_only_when_offline"     boolean NOT NULL DEFAULT true,
    "email_digest_frequency"      varchar NOT NULL DEFAULT 'off' CHECK (email_digest_frequency IN ('off', 'daily', 'weekly')),
    "quiet_hours_enabled"         boolean NOT NULL DEFAULT false,
    "quiet_hours_start"           varchar,           -- 'HH:MM'
    "quiet_hours_end"             varchar,           -- 'HH:MM'
    "quiet_hours_tz"              varchar,           -- IANA tz, e.g. 'Asia/Kolkata'
    "unsubscribe_token"           varchar NOT NULL UNIQUE,
    "created_at"                  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"                  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_users_notification_preferences_token
    ON users_notification_preferences(unsubscribe_token);

-- 2. Durable queue for outbound emails.
--    Mirrors github_sync_queue: status machine, attempts counter, next_retry_at,
--    error_message. dedup_key prevents N duplicates for the same logical event
--    (mention storms, multi-fan-out, retry cycles).
--
--    A partial unique index on dedup_key restricted to NOT-finalised rows lets
--    the same logical event re-enqueue once a previous attempt is completed
--    or has failed-out (so we don't permanently silence future mentions in
--    a thread).
CREATE TABLE IF NOT EXISTS notification_email_queue (
    "id"             uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "user_id"        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "to_email"       varchar NOT NULL,
    "event_type"     varchar NOT NULL,
    "subject"        varchar NOT NULL,
    "html_body"      text    NOT NULL,
    "text_body"      text    NOT NULL,
    "cta_url"        varchar,
    "dedup_key"      varchar NOT NULL,
    "status"         varchar NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'processing', 'sent', 'failed', 'suppressed')),
    "attempts"       int     NOT NULL DEFAULT 0,
    "next_retry_at"  TIMESTAMP WITH TIME ZONE,
    "error_message"  text,
    "metadata"       jsonb   NOT NULL DEFAULT '{}'::jsonb,
    "created_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_notification_email_queue_status_retry
    ON notification_email_queue(status, next_retry_at);
CREATE INDEX IF NOT EXISTS idx_notification_email_queue_user
    ON notification_email_queue(user_id, created_at DESC);
-- Block duplicate pending/processing rows for the same logical event.
-- Once a row reaches sent/failed/suppressed, the dedup_key is freed so a
-- later occurrence of the same event can enqueue again.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_notification_email_queue_active_dedup
    ON notification_email_queue(dedup_key)
    WHERE status IN ('pending', 'processing');

-- 3. Persistent log for admin observability and bounce/complaint correlation.
--    provider_message_id ties a row to Resend's message; webhook deliveries
--    update the corresponding row's status without scanning the whole table.
CREATE TABLE IF NOT EXISTS notification_email_log (
    "id"                  uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "user_id"             uuid REFERENCES users(id) ON DELETE SET NULL,
    "to_email"            varchar NOT NULL,
    "event_type"          varchar NOT NULL,
    "subject"             varchar NOT NULL,
    "status"              varchar NOT NULL,
    "provider_message_id" varchar,
    "error_message"       text,
    "queue_id"            uuid,
    "attempts"            int NOT NULL DEFAULT 1,
    "created_at"          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_notification_email_log_user_created
    ON notification_email_log(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_notification_email_log_provider
    ON notification_email_log(provider_message_id)
    WHERE provider_message_id IS NOT NULL;

-- 4. Suppression list for hard bounces and complaints.
--    Looked up before any send. Prevents domain-reputation damage and
--    the cost of repeatedly hitting a known-bad address.
CREATE TABLE IF NOT EXISTS notification_email_suppressions (
    "id"         uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "email"      varchar NOT NULL UNIQUE,
    "reason"     varchar NOT NULL CHECK (reason IN ('bounced', 'complained', 'manual', 'unsubscribed')),
    "details"    text,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_notification_email_suppressions_email
    ON notification_email_suppressions(email);
