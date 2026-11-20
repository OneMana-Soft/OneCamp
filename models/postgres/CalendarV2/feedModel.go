package models

// Secret feeds of calendars (calendar B8, the calendar_feeds migration): one
// person's link to a calendar they may see, found by its token's SHA-256 (the
// token itself is never kept), busy or full, revoked at once.

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
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

// Feed is one person's secret feed of a calendar, or (CalendarID uuid.Nil:
// a NULL calendar_id, migration 600) of everything they may see.
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
	// RevokedAt is when it stopped, and RevokedBy who stopped it (its person,
	// or a workspace admin); nil while it is live.
	RevokedAt *time.Time
	RevokedBy *uuid.UUID
	// LastReadAt is when the link was last read, to the hour (FeedRead).
	LastReadAt *time.Time
}

const feedColumns = `id, calendar_id, person_id, token_hash, scope, created_at, revoked_at, revoked_by, last_read_at`

// sameCalendar matches a feed's calendar_id to $1: a calendar, or NULL for a
// workspace feed (calendarArg).
const sameCalendar = `calendar_id IS NOT DISTINCT FROM $1::uuid`

// calendarArg is a feed's calendar as its column keeps it: NULL for a
// workspace feed. Pure.
func calendarArg(calendar uuid.UUID) any {
	if calendar == uuid.Nil {
		return nil
	}
	return calendar
}

func scanFeed(row pgexec.Row) (*Feed, error) {
	var f Feed
	var hash string
	var cal, by uuid.NullUUID
	var revoked, read sql.NullTime
	if err := row.Scan(&f.ID, &cal, &f.PersonID, &hash, &f.Scope, &f.CreatedAt, &revoked, &by, &read); err != nil {
		return nil, err
	}
	f.CalendarID = cal.UUID
	if revoked.Valid {
		at := revoked.Time
		f.RevokedAt = &at
	}
	if by.Valid {
		id := by.UUID
		f.RevokedBy = &id
	}
	if read.Valid {
		at := read.Time
		f.LastReadAt = &at
	}
	var err error
	f.TokenHash, err = hex.DecodeString(hash)
	return &f, err
}

// LatestFeed is person's live feed of a calendar (uuid.Nil: of everything),
// else the one that stopped last, or nil.
func LatestFeed(ctx context.Context, calendar, person uuid.UUID) (*Feed, error) {
	f, _, err := pgexec.QueryOne(ctx, `SELECT `+feedColumns+` FROM calendar_feeds
		WHERE `+sameCalendar+` AND person_id = $2 ORDER BY COALESCE(revoked_at, 'infinity') DESC LIMIT 1`, scanFeed, calendarArg(calendar), person)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.LatestFeed err: %+v", err)
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

// RevokeFeeds stops person's live feed of a calendar (uuid.Nil: of
// everything), if there is one, as by, and gives it as it was, its id and
// the scope it showed among it: nil when none was live (one live feed at
// most: calendar_feeds_live_idx and calendar_feeds_workspace_live_idx).
func RevokeFeeds(ctx context.Context, calendar, person, by uuid.UUID) (*Feed, error) {
	f, _, err := pgexec.QueryOne(ctx, `UPDATE calendar_feeds SET revoked_at = NOW(), revoked_by = $3
		WHERE `+sameCalendar+` AND person_id = $2 AND revoked_at IS NULL RETURNING `+feedColumns,
		scanFeed, calendarArg(calendar), person, by)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.RevokeFeeds err: %+v", err)
	}
	return f, err
}

// RevokeFeedByID stops one live feed, as by (a workspace admin), and gives
// it as it was; nil when no live feed has that id.
func RevokeFeedByID(ctx context.Context, id, by uuid.UUID) (*Feed, error) {
	f, _, err := pgexec.QueryOne(ctx, `UPDATE calendar_feeds SET revoked_at = NOW(), revoked_by = $2
		WHERE id = $1 AND revoked_at IS NULL RETURNING `+feedColumns, scanFeed, id, by)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.RevokeFeedByID err: %+v", err)
	}
	return f, err
}

// LiveFeeds are the workspace's live feeds, newest first, at most limit.
func LiveFeeds(ctx context.Context, limit int) ([]*Feed, error) {
	out, err := pgexec.Query(ctx, `SELECT `+feedColumns+` FROM calendar_feeds
		WHERE revoked_at IS NULL ORDER BY created_at DESC, id LIMIT $1`, scanFeed, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.LiveFeeds err: %+v", err)
	}
	return out, err
}

// FeedRead marks a live link read now, unless it was marked within every:
// one small write an hour at most however often a calendar app asks.
func FeedRead(ctx context.Context, id uuid.UUID, every time.Duration) error {
	_, err := pgexec.Exec(ctx, `UPDATE calendar_feeds SET last_read_at = NOW()
		WHERE id = $1 AND revoked_at IS NULL AND (last_read_at IS NULL OR last_read_at <= NOW() - $2::interval)`,
		id, fmt.Sprintf("%d seconds", int64(every/time.Second)))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.FeedRead err: %+v", err)
	}
	return err
}

// InsertFeed keeps a new live feed (the caller revokes the old one first, in
// the same transaction: pgexec.InTx).
func InsertFeed(ctx context.Context, f Feed) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO calendar_feeds (id, calendar_id, person_id, token_hash, scope) VALUES ($1, $2, $3, $4, $5)`,
		f.ID, calendarArg(f.CalendarID), f.PersonID, hex.EncodeToString(f.TokenHash), f.Scope)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CalendarV2.InsertFeed err: %+v", err)
	}
	return err
}
