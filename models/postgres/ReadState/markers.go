// Package models (ReadState) is where a person's read marker sits in a
// conversation, and what lies after it (chat A3, PROGRAM.md §6). A marker is
// the moment the person had read up to, in the conversation's own row times
// (posts.created_at, chats.created_at); the unread are the rows after it that
// are live and not the reader's own. One table descriptor per kind, so the
// counts, the first unread and Mark unread read one rule.
//
// Message precision without new columns: a row's id is its message's uuid,
// so "mark unread from this message" sets the marker a microsecond before
// that row's own time, and the first unread is the first row after the
// marker. The graph's message times aren't the rows' (each row takes
// Postgres' NOW()), which is why the marker is never set from the graph.
package models

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// Table is one kind's marker table and the rows it reads.
type Table struct {
	Marker string // the marker table: user_id, <Key>, user_last_seen
	Key    string // the conversation's column in Marker
	Rows   string // the messages' table: id, <RowKey>, created_by, created_at, deleted_at
	RowKey string // the conversation's column in Rows
	KeyOf  string // the key's SQL type, for its parameter
}

// Channel is a channel's posts; Chat is a DM's or a group's messages.
var (
	Channel = Table{Marker: "last_seen_channel", Key: "channel_id", Rows: "posts", RowKey: "post_channel", KeyOf: "uuid"}
	Chat    = Table{Marker: "last_seen_chat", Key: "grp_id", Rows: "chats", RowKey: "grp_id", KeyOf: "varchar"}
)

// Unread is the join condition for the rows a marker hasn't read, with the
// marker as lsc and the rows as m: after it, live, not the reader's own.
// ">" rather than ">=": a marker set as a message is read has read that one.
func (t Table) Unread() string {
	return `m.` + t.RowKey + ` = lsc.` + t.Key + `
			AND m.created_at > lsc.user_last_seen
			AND m.deleted_at IS NULL
			AND m.created_by <> lsc.user_id`
}

// ErrNoMessage is MarkUnreadFrom's answer for a message that isn't a live one
// of that conversation.
var ErrNoMessage = errors.New("readstate: no such message in the conversation")

// MarkUnreadFrom moves who's marker in the conversation to just before
// message, so it and everything after it (but who's own) is unread. It moves
// back as well as forward: Mark unread is the one move that may.
func MarkUnreadFrom(ctx context.Context, t Table, who uuid.UUID, conversation string, message uuid.UUID) error {
	q := `INSERT INTO ` + t.Marker + ` (user_id, ` + t.Key + `, user_last_seen)
		SELECT $1::uuid, $2::` + t.KeyOf + `, m.created_at - interval '1 microsecond'
		FROM ` + t.Rows + ` m
		WHERE m.id = $3::uuid AND m.` + t.RowKey + ` = $2::` + t.KeyOf + ` AND m.deleted_at IS NULL
		ON CONFLICT (user_id, ` + t.Key + `) DO UPDATE SET user_last_seen = EXCLUDED.user_last_seen`
	n, err := pgexec.Exec(ctx, q, who, conversation, message)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNoMessage
	}
	return nil
}

// Position is where who is in a conversation: how many messages they
// haven't read, the first of them, and the moment their marker stands at.
type Position struct {
	Unread      int
	FirstUnread string    // the first unread message's uuid; "" with none
	Since       time.Time // the marker; zero where who has none
}

// PositionOf is who's position in the conversation, in one read. With no
// marker, nothing is counted (as the sidebar's counts): a conversation never
// opened has no line.
func PositionOf(ctx context.Context, t Table, who uuid.UUID, conversation string) (Position, error) {
	q := `SELECT lsc.user_last_seen, m.id, COUNT(m.id) OVER ()
		FROM ` + t.Marker + ` lsc
		LEFT JOIN ` + t.Rows + ` m ON ` + t.Unread() + `
		WHERE lsc.user_id = $1::uuid AND lsc.` + t.Key + ` = $2::` + t.KeyOf + `
		ORDER BY m.created_at NULLS LAST, m.id
		LIMIT 1`
	p, _, err := pgexec.QueryOne(ctx, q, func(r pgexec.Row) (Position, error) {
		var (
			p     Position
			first uuid.NullUUID
		)
		if err := r.Scan(&p.Since, &first, &p.Unread); err != nil {
			return Position{}, err
		}
		if first.Valid {
			p.FirstUnread = first.UUID.String()
		}
		return p, nil
	}, who, conversation)
	return p, err
}
