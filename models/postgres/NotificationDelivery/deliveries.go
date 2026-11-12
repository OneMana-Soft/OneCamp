// Package models (NotificationDelivery) is the Notification Router's record of
// what it told whom (migration 585, business/Notify Deliver and Retract): one
// row per event, person and delivery. Ids only, never words.
package models

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
)

// Row is one person's one delivery of one event.
type Row struct {
	EventID   string
	UserID    string
	Delivery  string
	Cause     string
	State     string // "sent" or "held" when written
	MessageID string // "" when the event isn't about a message
	HoldUntil *time.Time
	SentAt    *time.Time
	CreatedAt time.Time // when it was written: read back, never written
}

// recordBatch is how many rows one statement writes (8 parameters each, well
// under Postgres's 65,535).
const recordBatch = 1000

// Record writes rows, each (event, person, delivery) once: one already
// written is left as it is, so a retry or a second fire of the same event
// writes nothing. It answers the rows it wrote, by lower-case user id and
// delivery.
func Record(ctx context.Context, rows []Row) ([]Row, error) {
	var fresh []Row
	for start := 0; start < len(rows); start += recordBatch {
		batch := rows[start:min(start+recordBatch, len(rows))]
		var values []string
		var args []any
		for i, r := range batch {
			n := i * 8
			values = append(values, fmt.Sprintf("($%d, $%d::uuid, $%d, $%d, $%d, NULLIF($%d, '')::uuid, $%d::timestamptz, $%d::timestamptz)",
				n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8))
			args = append(args, r.EventID, r.UserID, r.Delivery, r.Cause, r.State, r.MessageID, r.HoldUntil, r.SentAt)
		}
		q := `INSERT INTO notification_deliveries (event_id, user_id, delivery, cause, state, message_id, hold_until, sent_at)
			VALUES ` + strings.Join(values, ", ") + `
			ON CONFLICT (event_id, user_id, delivery) DO NOTHING
			RETURNING lower(user_id::text), delivery`
		got, err := pgexec.Query(ctx, q, func(r pgexec.Row) (out Row, err error) {
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
// sent is marked retracted, what was still held (never sent) dropped. Nothing
// is deleted. It answers whose rows it changed (user uuids, lower case, once
// each).
func Retract(ctx context.Context, messageID string) ([]string, error) {
	return pgexec.Query(ctx, `
		WITH taken AS (
			UPDATE notification_deliveries
			SET state = CASE WHEN state = 'held' THEN 'dropped' ELSE 'retracted' END
			WHERE message_id = $1::uuid AND state IN ('sent', 'held')
			RETURNING user_id)
		SELECT DISTINCT lower(user_id::text) FROM taken`, pgexec.Text, messageID)
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
