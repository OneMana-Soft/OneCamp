package models

// The messages ledger as a shared history writes it: one row per copy, claimed
// before the copy is written, so a share that runs again (a retry, a second
// worker) copies each message into a conversation once.

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Copy is one copy a share makes: the copy's id and the message it copies, in
// a conversation, by its author, at the original's time, in a thread when it
// is a reply (ThreadRoot is the copy's root).
type Copy struct {
	ID           uuid.UUID
	Conversation uuid.UUID
	Author       uuid.UUID
	CopiedFrom   uuid.UUID
	ThreadRoot   *uuid.UUID
	At           time.Time
}

// ClaimCopy writes c's ledger row unless the conversation has a copy of that
// message already, and says whether it did: only the share that claimed a
// copy writes it.
func ClaimCopy(ctx context.Context, c Copy) (bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	res, err := db().ExecContext(cctx, `
		INSERT INTO messages (id, conversation_id, thread_root, author_id, copied_from, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (conversation_id, copied_from) WHERE copied_from IS NOT NULL DO NOTHING`,
		c.ID, c.Conversation, c.ThreadRoot, c.Author, c.CopiedFrom, c.At)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseCopy takes back a claim whose copy couldn't be written, so the next
// run writes it.
func ReleaseCopy(ctx context.Context, id uuid.UUID) error {
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := db().ExecContext(cctx, `DELETE FROM messages WHERE id = $1`, id)
	return err
}

// CopiesOf is, for each of originals the conversation has a copy of, the
// copy's id.
func CopiesOf(ctx context.Context, conversation uuid.UUID, originals []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := map[uuid.UUID]uuid.UUID{}
	if len(originals) == 0 {
		return out, nil
	}
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT copied_from, id FROM messages
		WHERE conversation_id = $1 AND copied_from = ANY($2::uuid[])`, conversation, uuidArray(originals))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var from, id uuid.UUID
		if err := rows.Scan(&from, &id); err != nil {
			return nil, err
		}
		out[from] = id
	}
	return out, rows.Err()
}
