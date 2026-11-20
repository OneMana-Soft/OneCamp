-- 605 (linear-21, the mail door): a project's connected mailbox, which the
-- server reads over IMAP (business/TriageMail), and the Message-ID keys it has
-- taken in, so a message lands in triage once. The secret is sealed by
-- helpers/secretbox.go; removing a mailbox soft-deletes its row and wipes the
-- secret in the same statement.
CREATE TABLE IF NOT EXISTS triage_mailboxes (
    id             uuid PRIMARY KEY,
    project_uuid   uuid NOT NULL,
    host           varchar(253) NOT NULL,
    port           integer NOT NULL CHECK (port IN (993, 143)),
    tls_mode       varchar(8) NOT NULL CHECK (tls_mode IN ('tls', 'starttls')),
    username       varchar(320) NOT NULL,
    secret_sealed  text NOT NULL DEFAULT '',
    folder         varchar(200) NOT NULL DEFAULT 'INBOX',
    private_ok     boolean NOT NULL DEFAULT false,
    uid_validity   bigint NOT NULL DEFAULT 0,
    last_uid       bigint NOT NULL DEFAULT 0,
    failures       integer NOT NULL DEFAULT 0,
    paused_reason  varchar(16) CHECK (paused_reason IN ('admin', 'auth_failed', 'tls', 'refused', 'unreachable', 'triage_off', 'owner_left', 'project_deleted')),
    next_poll_at   timestamptz NOT NULL DEFAULT NOW(),
    leased_until   timestamptz,
    last_polled_at timestamptz,
    connected_by   uuid NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT NOW(),
    updated_at     timestamptz NOT NULL DEFAULT NOW(),
    deleted_at     timestamptz,
    deleted_by     uuid
);

-- One live mailbox a project; the poller's due scan.
CREATE UNIQUE INDEX IF NOT EXISTS triage_mailboxes_project_live ON triage_mailboxes (project_uuid) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS triage_mailboxes_due ON triage_mailboxes (next_poll_at) WHERE deleted_at IS NULL AND paused_reason IS NULL;

-- message_key is sha256 of the Message-ID (or of from, date, subject and the
-- body's hash when there is none): 32 bytes, never the user's text. A row
-- whose task_uuid is NULL was claimed and not finished; it may be claimed
-- again after 15 minutes (domain/TriageMail ClaimKeys).
CREATE TABLE IF NOT EXISTS triage_mail_seen (
    project_uuid uuid NOT NULL,
    message_key  bytea NOT NULL CHECK (octet_length(message_key) = 32),
    mailbox_uuid uuid NOT NULL,
    task_uuid    uuid,
    claimed_at   timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_uuid, message_key)
);

-- One statement drops the old check and adds the new one.
ALTER TABLE triage_items
    DROP CONSTRAINT IF EXISTS triage_items_source_check,
    ADD CONSTRAINT triage_items_source_check CHECK (source IN ('form', 'api', 'agent', 'github', 'chat', 'import', 'mail'));
