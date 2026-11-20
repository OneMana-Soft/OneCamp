package models

// The calls a table links to, and the calls a table's picker offers
// (business/CallState callTitles.go): each read is one statement, whatever
// number of calls it names. The statements are domain/CallState's; they run here.

import (
	"context"
	"time"

	callStateDomain "github.com/akashc777/OneCamp/domain/CallState"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// SessionsByIDs is the calls of ids, read in one statement; an id no call has
// is left out.
func SessionsByIDs(ctx context.Context, ids []uuid.UUID) ([]Session, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, callStateDomain.SessionsByIDsQuery, scanSessionRow, pq.Array(ids))
}

// SessionsAfter is where a page of calls, newest first, carries on from: the
// last one read's start and id. The zero value starts at the newest.
type SessionsAfter struct {
	StartedAt time.Time
	ID        uuid.UUID
}

// SessionsBefore is a page of up to size calls of the conversations reader
// is in, newest first, that started before after (every call, for its zero
// value), and where the next page carries on from.
func SessionsBefore(ctx context.Context, reader uuid.UUID, after SessionsAfter, size int) ([]Session, SessionsAfter, error) {
	if after.StartedAt.IsZero() {
		after = SessionsAfter{StartedAt: time.Now().Add(time.Hour), ID: uuid.Nil}
	}
	if reader == uuid.Nil || size < 1 {
		return nil, after, nil
	}
	rows, err := pgexec.Query(ctx, callStateDomain.SessionsBeforeQuery, scanSessionRow, reader, after.StartedAt, after.ID, size)
	if err != nil || len(rows) == 0 {
		return rows, after, err
	}
	last := rows[len(rows)-1]
	return rows, SessionsAfter{StartedAt: last.StartedAt, ID: last.ID}, nil
}

func scanSessionRow(r pgexec.Row) (Session, error) {
	s, err := scanSession(r)
	if err != nil {
		return Session{}, err
	}
	return *s, nil
}
