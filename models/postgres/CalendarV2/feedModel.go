package models

// Secret feeds of calendars (calendar B8, the calendar_feeds migration): one
// person's link to a calendar they may see, found by its token's SHA-256 (the
// token itself is never kept), busy or full, revoked at once.

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// The scopes a feed is made at: when only, or what too.
const (
	FeedBusy = "busy"
	FeedFull = "full"
)

// Feed is one person's secret feed of a calendar.
type Feed struct {
	ID         uuid.UUID
	CalendarID uuid.UUID
	PersonID   uuid.UUID
	// TokenHash is the hash its link is found by (helpers.LinkSecretHash),
	// kept in token_hash as hex: the text every feed has kept since
	// migration 480, so links made before are found the same way.
	TokenHash []byte
	Scope     string
	CreatedAt time.Time
}

const feedColumns = `id, calendar_id, person_id, token_hash, scope, created_at`

func scanFeed(row pgexec.Row) (*Feed, error) {
	var f Feed
	var hash string
	if err := row.Scan(&f.ID, &f.CalendarID, &f.PersonID, &hash, &f.Scope, &f.CreatedAt); err != nil {
		return nil, err
	}
	var err error
	f.TokenHash, err = hex.DecodeString(hash)
	return &f, err
}

// LiveFeed is person's live feed of a calendar, or nil.
func LiveFeed(ctx context.Context, calendar, person uuid.UUID) (*Feed, error) {
	f, _, err := pgexec.QueryOne(ctx, `SELECT `+feedColumns+` FROM calendar_feeds
		WHERE calendar_id = $1 AND person_id = $2 AND revoked_at IS NULL`, scanFeed, calendar, person)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.LiveFeed err: %+v", err)
	}
	return f, err
}

// FeedByHash is the live feed a token's hash finds, or nil.
func FeedByHash(ctx context.Context, hash []byte) (*Feed, error) {
	f, _, err := pgexec.QueryOne(ctx, `SELECT `+feedColumns+` FROM calendar_feeds
		WHERE token_hash = $1 AND revoked_at IS NULL`, scanFeed, hex.EncodeToString(hash))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.FeedByHash err: %+v", err)
	}
	return f, err
}

// RevokeFeeds stops person's live feed of a calendar, if there is one.
func RevokeFeeds(ctx context.Context, calendar, person uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE calendar_feeds SET revoked_at = NOW() WHERE calendar_id = $1 AND person_id = $2 AND revoked_at IS NULL`, calendar, person)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.RevokeFeeds err: %+v", err)
	}
	return err
}

// InsertFeed keeps a new live feed (the caller revokes the old one first, in
// the same transaction: pgexec.InTx).
func InsertFeed(ctx context.Context, f Feed) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO calendar_feeds (id, calendar_id, person_id, token_hash, scope) VALUES ($1, $2, $3, $4, $5)`,
		f.ID, f.CalendarID, f.PersonID, hex.EncodeToString(f.TokenHash), f.Scope)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.InsertFeed err: %+v", err)
	}
	return err
}
