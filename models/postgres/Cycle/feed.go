package models

// Secret calendar links to a project's cycles (linear-12 part 4, the
// cycle_feeds migration): one person's link per project, found by its
// token's SHA-256 (the token itself is never kept), revoked at once.

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// Feed is one person's secret link to a project's cycles.
type Feed struct {
	ID          uuid.UUID
	ProjectUUID uuid.UUID
	PersonID    uuid.UUID
	// TokenHash is the hash its link is found by (helpers.LinkSecretHash),
	// kept as hex as a calendar's feed keeps it.
	TokenHash []byte
	CreatedAt time.Time
}

const feedColumns = `id, project_uuid, person_id, token_hash, created_at`

func scanFeed(row pgexec.Row) (*Feed, error) {
	var f Feed
	var hash string
	if err := row.Scan(&f.ID, &f.ProjectUUID, &f.PersonID, &hash, &f.CreatedAt); err != nil {
		return nil, err
	}
	var err error
	f.TokenHash, err = hex.DecodeString(hash)
	return &f, err
}

// LiveFeed is person's live link to a project's cycles, or nil.
func LiveFeed(ctx context.Context, project, person uuid.UUID) (*Feed, error) {
	f, _, err := pgexec.QueryOne(ctx, `SELECT `+feedColumns+` FROM cycle_feeds
		WHERE project_uuid = $1 AND person_id = $2 AND revoked_at IS NULL`, scanFeed, project, person)
	return f, err
}

// FeedByHash is the live link a token's hash finds, or nil.
func FeedByHash(ctx context.Context, hash []byte) (*Feed, error) {
	f, _, err := pgexec.QueryOne(ctx, `SELECT `+feedColumns+` FROM cycle_feeds
		WHERE token_hash = $1 AND revoked_at IS NULL`, scanFeed, hex.EncodeToString(hash))
	return f, err
}

// RevokeFeed stops person's live link to a project's cycles, if there is one.
func RevokeFeed(ctx context.Context, project, person uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE cycle_feeds SET revoked_at = NOW() WHERE project_uuid = $1 AND person_id = $2 AND revoked_at IS NULL`, project, person)
	return err
}

// MakeFeed keeps f as person's one live link to the project's cycles,
// revoking the one they had, in one transaction.
func MakeFeed(ctx context.Context, f Feed) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		if err := RevokeFeed(ctx, f.ProjectUUID, f.PersonID); err != nil {
			return err
		}
		_, err := pgexec.Exec(ctx, `INSERT INTO cycle_feeds (id, project_uuid, person_id, token_hash) VALUES ($1, $2, $3, $4)`,
			f.ID, f.ProjectUUID, f.PersonID, hex.EncodeToString(f.TokenHash))
		return err
	})
}
