// Package domain (NotificationEmailQueue) is the query text of the email
// queue's claim and outcomes, and of its log: business/Notification owns
// their rules, and models/postgres/NotificationEmailQueue and
// models/postgres/NotificationEmailLog run the text.
package domain

import (
	"fmt"
	"strings"
)

// columns is a queue row's columns, in the order the model scans them.
const columns = `id, user_id, to_email, event_type, subject, html_body, text_body,
	cta_url, dedup_key, status, attempts, next_retry_at, error_message,
	metadata, created_at, updated_at`

// Due is up to $1 pending rows due now, oldest first, skipping any another
// worker holds; the claim's tx locks them.
const Due = `SELECT ` + columns + `
	FROM notification_email_queue
	WHERE status = 'pending'
	  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
	ORDER BY created_at ASC
	LIMIT $1
	FOR UPDATE SKIP LOCKED`

// WindowMates is, in the claim's tx, the window-mates of the rows it claimed:
// the pending rows of the people $1 (uuid[]) of the kinds $2 (text[]) due
// within $4 (an interval) from now, but for the rows $3 (uuid[]) already
// claimed; at most $5, oldest first, skipping any another worker holds.
// A person's rows are found by idx_notification_email_queue_user.
const WindowMates = `SELECT ` + columns + `
	FROM notification_email_queue
	WHERE status = 'pending'
	  AND user_id = ANY($1::uuid[])
	  AND event_type = ANY($2::text[])
	  AND id <> ALL($3::uuid[])
	  AND COALESCE(next_retry_at, NOW()) <= NOW() + $4::interval
	ORDER BY created_at ASC
	LIMIT $5
	FOR UPDATE SKIP LOCKED`

// Claim takes the rows $1 (uuid[]) for a send, counting the attempt.
const Claim = `UPDATE notification_email_queue
	SET status = 'processing', attempts = attempts + 1, updated_at = NOW()
	WHERE id = ANY($1::uuid[])`

// Sent closes the rows $1 (uuid[]) as sent.
const Sent = `UPDATE notification_email_queue
	SET status = 'sent', error_message = NULL, updated_at = NOW()
	WHERE id = ANY($1::uuid[])`

// Closed closes the rows $1 (uuid[]) as $2 ('failed' or 'suppressed'),
// saying why in $3.
const Closed = `UPDATE notification_email_queue
	SET status = $2, error_message = $3, updated_at = NOW()
	WHERE id = ANY($1::uuid[])`

// Later puts the rows $1 (uuid[]) back to wait until $3, saying why in $2.
const Later = `UPDATE notification_email_queue
	SET status = 'pending', error_message = $2, next_retry_at = $3, updated_at = NOW()
	WHERE id = ANY($1::uuid[])`

// LogParams is how many parameters each row of Log takes.
const LogParams = 9

// Log writes rows rows to the email log. Each row's parameters, in order:
// the person (NULL for none), the address, the kind, the subject, the
// status, the provider's message id, the error, the queue row (each NULL for
// none) and the attempts.
func Log(rows int) string {
	values := make([]string, rows)
	for i := range values {
		n := i * LogParams
		values[i] = fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9)
	}
	return `INSERT INTO notification_email_log
		(user_id, to_email, event_type, subject, status, provider_message_id, error_message, queue_id, attempts)
		VALUES ` + strings.Join(values, ", ")
}
