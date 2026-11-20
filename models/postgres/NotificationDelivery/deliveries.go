// Package models (NotificationDelivery) is the Notification Router's record of
// what it told whom (migrations 585 and 592, business/Notify Deliver, Retract
// and the waiting pushes' Sweeper): one row per event, person and delivery.
// Ids only, never words.
package models

import (
	"context"
	"time"

	deliveryDomain "github.com/akashc777/OneCamp/domain/NotificationDelivery"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/lib/pq"
)

// Row is one person's one delivery of one event.
type Row struct {
	EventID   string
	UserID    string
	Delivery  string
	Cause     string
	State     string // "sent", "held" or "waiting" when written
	MessageID string // "" when the event isn't about a message
	HoldUntil *time.Time
	SentAt    *time.Time
	// CreatedAt is when it was written: zero writes now (the column's
	// default); one of a message written earlier (an import, the demo's
	// seed) is written at the message's time.
	CreatedAt time.Time
}

// recordBatch is how many rows one statement writes (deliveryDomain's
// RecordParams each, well under Postgres's 65,535).
const recordBatch = 1000

// Record writes rows, each (event, person, delivery) once: one already
// written is left as it is, so a retry or a second fire of the same event
// writes nothing. It answers the rows it wrote, by lower-case user id and
// delivery.
func Record(ctx context.Context, rows []Row) ([]Row, error) {
	var fresh []Row
	for start := 0; start < len(rows); start += recordBatch {
		batch := rows[start:min(start+recordBatch, len(rows))]
		args := make([]any, 0, len(batch)*deliveryDomain.RecordParams)
		for _, r := range batch {
			var written *time.Time
			if at := r.CreatedAt; !at.IsZero() {
				written = &at
			}
			args = append(args, r.EventID, r.UserID, r.Delivery, r.Cause, r.State, r.MessageID, r.HoldUntil, r.SentAt, written)
		}
		got, err := pgexec.Query(ctx, deliveryDomain.Record(len(batch)), func(r pgexec.Row) (out Row, err error) {
			err = r.Scan(&out.UserID, &out.Delivery)
			return
		}, args...)
		if err != nil {
			return fresh, err
		}
		fresh = append(fresh, got...)
	}
	return fresh, nil
}

// Retract takes back the deliveries of the events about messageID: what was
// sent is marked retracted, what was still held or waiting (never sent)
// dropped. Nothing is deleted. It answers the rows it changed (their event,
// lower-case user id, delivery and state now).
func Retract(ctx context.Context, messageID string) ([]Row, error) {
	return pgexec.Query(ctx, `
		UPDATE notification_deliveries
		SET state = CASE WHEN state IN ('held', 'waiting') THEN 'dropped' ELSE 'retracted' END
		WHERE message_id = $1::uuid AND state IN ('sent', 'held', 'waiting')
		RETURNING event_id, lower(user_id::text), delivery, state`, scanTaken, messageID)
}

// ClaimDue takes up to limit waiting rows whose time has come, oldest due
// first, each at most once whoever else claims at the same moment: a row
// another claim holds is skipped (SKIP LOCKED) and only a row still waiting
// is taken. Taken rows are marked sent now, before anything is sent, so a
// crash after the claim loses that push and never sends it twice. It answers
// the rows taken, with when they were due.
func ClaimDue(ctx context.Context, limit int) ([]Row, error) {
	return pgexec.Query(ctx, `
		UPDATE notification_deliveries d
		SET state = 'sent', sent_at = NOW()
		FROM (SELECT event_id, user_id, delivery FROM notification_deliveries
			WHERE state = 'waiting' AND hold_until <= NOW()
			ORDER BY hold_until
			LIMIT $1
			FOR UPDATE SKIP LOCKED) due
		WHERE d.event_id = due.event_id AND d.user_id = due.user_id AND d.delivery = due.delivery AND d.state = 'waiting'
		RETURNING d.event_id, lower(d.user_id::text), d.delivery, d.state`, scanTaken, limit)
}

// Settle moves rows from state from to state to, each when it is still in
// from, in one statement, and says how many moved. A row sent now is stamped
// sent now, and one that was waiting never waited (it went at once); any
// other state has no sent time.
func Settle(ctx context.Context, rows []Row, from, to string) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	events, users, deliveries := make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows))
	for i, r := range rows {
		events[i], users[i], deliveries[i] = r.EventID, r.UserID, r.Delivery
	}
	return pgexec.Exec(ctx, `
		UPDATE notification_deliveries d
		SET state = $5::text,
			sent_at = CASE WHEN $5::text = 'sent' THEN NOW() END,
			hold_until = CASE WHEN $4::text = 'waiting' AND $5::text = 'sent' THEN NULL ELSE d.hold_until END
		FROM unnest($1::text[], $2::uuid[], $3::text[]) AS k(event_id, user_id, delivery)
		WHERE d.event_id = k.event_id AND d.user_id = k.user_id AND d.delivery = k.delivery AND d.state = $4::text`,
		pq.Array(events), pq.Array(users), pq.Array(deliveries), from, to)
}

// Waiting is who of an event's people still has its push waiting, by
// lower-case user id: read by the event's key, one statement.
func Waiting(ctx context.Context, eventID string) ([]string, error) {
	return pgexec.Query(ctx, `
		SELECT lower(user_id::text) FROM notification_deliveries
		WHERE event_id = $1 AND delivery = 'push' AND state = 'waiting'`, pgexec.Text, eventID)
}

// scanTaken reads a row Retract or ClaimDue changed.
func scanTaken(r pgexec.Row) (out Row, err error) {
	err = r.Scan(&out.EventID, &out.UserID, &out.Delivery, &out.State)
	return
}

// OfMessage is one person's rows of the events about a message, newest
// first: what the router told them of it (business/Notify Why). Only
// userID's rows are read, by the message's index.
func OfMessage(ctx context.Context, userID, messageID string) ([]Row, error) {
	return pgexec.Query(ctx, `
		SELECT event_id, lower(user_id::text), delivery, cause, state, COALESCE(message_id::text, ''), hold_until, sent_at, created_at
		FROM notification_deliveries
		WHERE message_id = $1::uuid AND user_id = $2::uuid
		ORDER BY created_at DESC, event_id, delivery
		LIMIT $3`, func(r pgexec.Row) (out Row, err error) {
		err = r.Scan(&out.EventID, &out.UserID, &out.Delivery, &out.Cause, &out.State, &out.MessageID, &out.HoldUntil, &out.SentAt, &out.CreatedAt)
		return
	}, messageID, userID, ofMessageMax)
}

// ofMessageMax is the most rows OfMessage answers: every delivery of a few
// dozen events, more than one person is ever told of one message.
const ofMessageMax = 120
