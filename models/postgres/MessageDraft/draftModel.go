package models

// Postgres access for drafts kept on the server
// (migrations/*_message_drafts.up.sql): one row for each place a person has
// started a message and not sent it. business/Draft owns the rules (what a
// draft key names, who may keep one there, how big it may be); this file only
// reads and writes rows, through models/pgexec.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// ErrFull is a person who keeps as many drafts as they may: a new one is
// refused, and the ones they keep are left as they are.
var ErrFull = errors.New("the person keeps as many drafts as they may")

// Draft is one message_drafts row. ReplyTo and Files are the JSON the app
// wrote, kept as it is; nil for none.
type Draft struct {
	Key       string
	HTML      string
	ReplyTo   json.RawMessage
	Files     json.RawMessage
	AlsoSend  bool
	UpdatedAt time.Time
}

// nullJSON is raw as a jsonb parameter: SQL NULL for none.
func nullJSON(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return string(raw)
}

// Put keeps d as user's draft at d.Key, the one there before replaced, and
// answers when it was kept. A draft at a new key is refused with ErrFull once
// user keeps max; a draft they already keep may always change.
func Put(ctx context.Context, user uuid.UUID, d Draft, max int) (time.Time, error) {
	at, kept, err := pgexec.QueryOne(ctx, `
		INSERT INTO message_drafts (user_id, draft_key, body_html, reply_to, files, also_send, updated_at)
		SELECT $1, $2, $3, $4::jsonb, $5::jsonb, $6, NOW()
		WHERE EXISTS (SELECT 1 FROM message_drafts WHERE user_id = $1 AND draft_key = $2)
		   OR (SELECT count(*) FROM message_drafts WHERE user_id = $1) < $7
		ON CONFLICT (user_id, draft_key) DO UPDATE
		   SET body_html = EXCLUDED.body_html, reply_to = EXCLUDED.reply_to, files = EXCLUDED.files,
		       also_send = EXCLUDED.also_send, updated_at = NOW()
		RETURNING updated_at`, func(r pgexec.Row) (at time.Time, err error) {
		err = r.Scan(&at)
		return
	}, user, d.Key, d.HTML, nullJSON(d.ReplyTo), nullJSON(d.Files), d.AlsoSend, max)
	if err != nil {
		return time.Time{}, err
	}
	if !kept {
		return time.Time{}, ErrFull
	}
	return at, nil
}

// Delete forgets user's draft at key, and says whether there was one.
func Delete(ctx context.Context, user uuid.UUID, key string) (bool, error) {
	n, err := pgexec.Exec(ctx, `DELETE FROM message_drafts WHERE user_id = $1 AND draft_key = $2`, user, key)
	return n == 1, err
}

// List is user's drafts, newest first, at most limit.
func List(ctx context.Context, user uuid.UUID, limit int) ([]Draft, error) {
	return pgexec.Query(ctx, `
		SELECT draft_key, body_html, reply_to, files, also_send, updated_at FROM message_drafts
		WHERE user_id = $1
		ORDER BY updated_at DESC, draft_key
		LIMIT $2`, func(r pgexec.Row) (d Draft, err error) {
		var reply, files []byte
		if err = r.Scan(&d.Key, &d.HTML, &reply, &files, &d.AlsoSend, &d.UpdatedAt); err != nil {
			return d, err
		}
		d.ReplyTo, d.Files = reply, files
		return d, nil
	}, user, limit)
}
