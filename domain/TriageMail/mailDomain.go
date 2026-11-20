// Package domain (TriageMail) is the query text of the mail door: a project's
// connected mailbox (triage_mailboxes, migration 605) and the Message-ID keys
// it took in (triage_mail_seen). business/TriageMail owns the rules and
// models/postgres/TriageMail runs the text through models/pgexec.
package domain

// mailboxColumns is what a read of a mailbox returns, in Mailbox's order.
const mailboxColumns = `id, project_uuid, host, port, tls_mode, username, secret_sealed, folder, private_ok,
	uid_validity, last_uid, failures, COALESCE(paused_reason, ''), last_polled_at, connected_by, updated_at`

// MailboxOf is a project's ($1) live mailbox.
const MailboxOf = `SELECT ` + mailboxColumns + ` FROM triage_mailboxes WHERE project_uuid = $1 AND deleted_at IS NULL`

// SaveMailbox connects a project's mailbox or changes it ($1 id, $2 project,
// $3 host, $4 port, $5 tls, $6 user, $7 sealed secret, "" keeps the one kept
// only while host, port, tls and user are unchanged (business keepsPassword),
// $8 folder, $9 private_ok, $10 paused reason or NULL, $11 by). A change of
// host, user or folder starts the mailbox again from what arrives next.
const SaveMailbox = `INSERT INTO triage_mailboxes (id, project_uuid, host, port, tls_mode, username, secret_sealed, folder, private_ok, paused_reason, connected_by)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	ON CONFLICT (project_uuid) WHERE deleted_at IS NULL DO UPDATE SET
		uid_validity = CASE WHEN (triage_mailboxes.host, triage_mailboxes.username, triage_mailboxes.folder) = (EXCLUDED.host, EXCLUDED.username, EXCLUDED.folder)
			THEN triage_mailboxes.uid_validity ELSE 0 END,
		host = EXCLUDED.host, port = EXCLUDED.port, tls_mode = EXCLUDED.tls_mode, username = EXCLUDED.username,
		secret_sealed = CASE WHEN EXCLUDED.secret_sealed = '' AND (triage_mailboxes.host, triage_mailboxes.port, triage_mailboxes.tls_mode, triage_mailboxes.username) =
			(EXCLUDED.host, EXCLUDED.port, EXCLUDED.tls_mode, EXCLUDED.username) THEN triage_mailboxes.secret_sealed ELSE EXCLUDED.secret_sealed END,
		folder = EXCLUDED.folder, private_ok = EXCLUDED.private_ok, paused_reason = EXCLUDED.paused_reason,
		failures = 0, next_poll_at = NOW(), connected_by = EXCLUDED.connected_by, updated_at = NOW()`

// PausedProjectDeleted is a mailbox its project's delete stopped: its
// password is wiped, so a restored project's admin enters it again.
const PausedProjectDeleted = "project_deleted"

// StopForProject stops a deleted project's ($1) mailbox, in the delete's own
// transaction (business/Project ArchiveProjectByProjectUUID): paused, so the
// due scan never claims it, and its sealed secret wiped. Restoring the
// project leaves it so, until an admin saves it with the password. updated_at
// moves, so a poll in flight records nothing (FinishPoll).
const StopForProject = `UPDATE triage_mailboxes SET secret_sealed = '', paused_reason = '` + PausedProjectDeleted + `', updated_at = NOW()
	WHERE project_uuid = $1 AND deleted_at IS NULL`

// RemoveMailbox disconnects a project's ($1) mailbox, by $2: a soft delete
// that wipes the sealed secret in the same statement, so nothing can read it
// again and the due scan (deleted_at IS NULL) never claims it.
const RemoveMailbox = `UPDATE triage_mailboxes SET secret_sealed = '', deleted_at = NOW(), deleted_by = $2, leased_until = NULL, updated_at = NOW()
	WHERE project_uuid = $1 AND deleted_at IS NULL`

// ClaimDue leases at most $2 mailboxes that are due, not paused, not removed
// and not leased by another server, until $1 (one statement: no lock is held
// while a mailbox is read over the network).
const ClaimDue = `UPDATE triage_mailboxes SET leased_until = $1 WHERE id IN (
		SELECT id FROM triage_mailboxes
		WHERE next_poll_at <= NOW() AND deleted_at IS NULL AND paused_reason IS NULL AND (leased_until IS NULL OR leased_until < NOW())
		ORDER BY next_poll_at LIMIT $2 FOR UPDATE SKIP LOCKED)
	RETURNING ` + mailboxColumns

// ClaimKeys claims a poll's Message-ID keys ($3, sha256 each) in a project
// ($1) for a mailbox ($2) and answers, in one statement, each key with
// whether this claim took it (fresh) and whether a task was made from it
// already (made): the unique key is the claim, so two pollers never both take
// a message. A key neither fresh nor made is another poll's live claim. A key
// claimed and never finished (task_uuid NULL) is claimed again after 15
// minutes, or at once when a poll released it (ReleaseKeys).
const ClaimKeys = `WITH claimed AS (
	INSERT INTO triage_mail_seen (project_uuid, message_key, mailbox_uuid)
	SELECT $1::uuid, k, $2::uuid FROM unnest($3::bytea[]) AS k
	ON CONFLICT (project_uuid, message_key) DO UPDATE SET claimed_at = NOW(), mailbox_uuid = EXCLUDED.mailbox_uuid
		WHERE triage_mail_seen.task_uuid IS NULL AND triage_mail_seen.claimed_at < NOW() - INTERVAL '15 minutes'
	RETURNING message_key)
SELECT k, EXISTS (SELECT 1 FROM claimed c WHERE c.message_key = k),
	EXISTS (SELECT 1 FROM triage_mail_seen s WHERE s.project_uuid = $1 AND s.message_key = k AND s.task_uuid IS NOT NULL)
FROM unnest($3::bytea[]) AS k`

// ReleaseKeys gives back a mailbox's ($2) claims in a project ($1) on keys
// ($3) it made no task from, so the next poll takes them at once.
const ReleaseKeys = `UPDATE triage_mail_seen SET claimed_at = 'epoch'
	WHERE project_uuid = $1 AND mailbox_uuid = $2 AND message_key = ANY($3::bytea[]) AND task_uuid IS NULL`

// FinishKeys names the task each claimed key ($2) made ($3, in step) in a
// project ($1).
const FinishKeys = `UPDATE triage_mail_seen AS s SET task_uuid = d.t
	FROM unnest($2::bytea[], $3::uuid[]) AS d(k, t) WHERE s.project_uuid = $1 AND s.message_key = d.k`

// FinishPoll records a poll that read a mailbox ($1): its UIDVALIDITY ($2),
// the last UID handled ($3), and when it is due again ($4), only on the row
// as the poll claimed it ($5, its updated_at): a save while the poll was out
// (another host, a pause) or its project's delete wins, this record is
// dropped and the lease lapses on its own.
const FinishPoll = `UPDATE triage_mailboxes SET uid_validity = $2, last_uid = $3, next_poll_at = $4, failures = 0,
	last_polled_at = NOW(), leased_until = NULL WHERE id = $1 AND updated_at = $5 AND deleted_at IS NULL`

// SetState records a poll that didn't read a mailbox ($1): its failures ($2),
// a pause's reason ($3, NULL for none) and when it is due again ($4), only on
// the row as the poll claimed it ($5), as FinishPoll.
const SetState = `UPDATE triage_mailboxes SET failures = $2, paused_reason = $3, next_poll_at = $4, leased_until = NULL, updated_at = NOW()
	WHERE id = $1 AND updated_at = $5 AND deleted_at IS NULL`
