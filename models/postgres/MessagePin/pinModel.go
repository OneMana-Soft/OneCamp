package models

// Postgres access for pinned messages (migrations/*_message_pins.up.sql): one
// row for each message pinned to its conversation. business/Pin owns the
// rules (who may pin, which messages, how many); this file only reads and
// writes rows, through models/pgexec.

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ErrFull is a conversation that holds as many pins as it may.
var ErrFull = errors.New("the conversation holds as many pins as it may")

// Pin is one message_pins row.
type Pin struct {
	MessageID uuid.UUID
	PinnedBy  uuid.UUID
	PinnedAt  time.Time
}

// Add pins message in the conversation as by, unless the conversation holds
// max pins already (ErrFull). A message already pinned stays as it was:
// added is false.
func Add(ctx context.Context, kind, id string, message, by uuid.UUID, max int) (added bool, err error) {
	n, err := pgexec.Exec(ctx, `
		INSERT INTO message_pins (conversation_kind, conversation_id, message_id, pinned_by, pinned_at)
		SELECT $1, $2, $3, $4, NOW()
		WHERE (SELECT count(*) FROM message_pins WHERE conversation_kind = $1 AND conversation_id = $2) < $5
		ON CONFLICT (conversation_kind, conversation_id, message_id) DO NOTHING`,
		kind, id, message, by, max)
	if err != nil || n == 1 {
		return n == 1, err
	}
	// Nothing written: pinned already, or full.
	pinned, err := pgexec.Scalar[bool](ctx, `
		SELECT EXISTS (SELECT 1 FROM message_pins WHERE conversation_kind = $1 AND conversation_id = $2 AND message_id = $3)`,
		kind, id, message)
	if err != nil {
		return false, err
	}
	if pinned {
		return false, nil
	}
	return false, ErrFull
}

// Row is a pin to write: a message in its conversation, who pinned it and
// when.
type Row struct {
	Kind, ID string
	Message  uuid.UUID
	By       uuid.UUID
	At       time.Time
}

// AddAll pins rows in one statement, as their pinners at their times, for an
// import: each conversation up to max pins, its earliest pins first. It
// answers the messages pinned now, written here or pinned already; a row past
// its conversation's room is left out. A message named twice is written once,
// at its earliest time.
func AddAll(ctx context.Context, rows []Row, max int) (map[uuid.UUID]bool, error) {
	pinned := map[uuid.UUID]bool{}
	if len(rows) == 0 {
		return pinned, nil
	}
	kinds, ids := make([]string, len(rows)), make([]string, len(rows))
	messages, by := make([]uuid.UUID, len(rows)), make([]uuid.UUID, len(rows))
	at := make([]string, len(rows))
	for i, r := range rows {
		kinds[i], ids[i], messages[i], by[i] = r.Kind, r.ID, r.Message, r.By
		at[i] = r.At.UTC().Format(time.RFC3339Nano)
	}
	got, err := pgexec.Query(ctx, `
		WITH want AS (
			SELECT DISTINCT ON (w.kind, w.conv, w.message) w.kind, w.conv, w.message, w.pinner, w.pinned_at::timestamptz AS pinned_at
			FROM unnest($1::text[], $2::text[], $3::uuid[], $4::uuid[], $5::text[]) AS w(kind, conv, message, pinner, pinned_at)
			ORDER BY w.kind, w.conv, w.message, w.pinned_at::timestamptz
		), fresh AS (
			SELECT w.*, row_number() OVER (PARTITION BY w.kind, w.conv ORDER BY w.pinned_at, w.message) AS n
			FROM want w
			WHERE NOT EXISTS (SELECT 1 FROM message_pins p
				WHERE p.conversation_kind = w.kind AND p.conversation_id = w.conv AND p.message_id = w.message)
		), added AS (
			INSERT INTO message_pins (conversation_kind, conversation_id, message_id, pinned_by, pinned_at)
			SELECT f.kind, f.conv, f.message, f.pinner, f.pinned_at FROM fresh f
			WHERE (SELECT count(*) FROM message_pins p WHERE p.conversation_kind = f.kind AND p.conversation_id = f.conv) + f.n <= $6
			ON CONFLICT (conversation_kind, conversation_id, message_id) DO NOTHING
			RETURNING message_id
		)
		SELECT message_id FROM added
		UNION
		SELECT w.message FROM want w JOIN message_pins p
			ON p.conversation_kind = w.kind AND p.conversation_id = w.conv AND p.message_id = w.message`,
		func(r pgexec.Row) (id uuid.UUID, err error) {
			err = r.Scan(&id)
			return
		}, pq.Array(kinds), pq.Array(ids), pq.Array(messages), pq.Array(by), pq.Array(at), max)
	if err != nil {
		return nil, err
	}
	for _, id := range got {
		pinned[id] = true
	}
	return pinned, nil
}

// Remove unpins message in the conversation, and says whether it was pinned.
func Remove(ctx context.Context, kind, id string, message uuid.UUID) (bool, error) {
	n, err := pgexec.Exec(ctx, `
		DELETE FROM message_pins WHERE conversation_kind = $1 AND conversation_id = $2 AND message_id = $3`,
		kind, id, message)
	return n == 1, err
}

// List is the conversation's pins, newest first, at most limit.
func List(ctx context.Context, kind, id string, limit int) ([]Pin, error) {
	return pgexec.Query(ctx, `
		SELECT message_id, pinned_by, pinned_at FROM message_pins
		WHERE conversation_kind = $1 AND conversation_id = $2
		ORDER BY pinned_at DESC, message_id
		LIMIT $3`, func(r pgexec.Row) (p Pin, err error) {
		err = r.Scan(&p.MessageID, &p.PinnedBy, &p.PinnedAt)
		return
	}, kind, id, limit)
}

// Forget drops the pins of messages that are gone from the conversation.
func Forget(ctx context.Context, kind, id string, messages []uuid.UUID) error {
	if len(messages) == 0 {
		return nil
	}
	_, err := pgexec.Exec(ctx, `
		DELETE FROM message_pins
		WHERE conversation_kind = $1 AND conversation_id = $2 AND message_id = ANY($3::uuid[])`,
		kind, id, pq.Array(messages))
	return err
}
