package domain

import (
	"context"
	"database/sql"
	"errors"

	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// ErrNoActor is a person who can't act: not found, or no longer active.
var ErrNoActor = errors.New("that person can't act any more")

// ActorByUUID is a person as the business layer takes an actor: their
// Postgres row (with the admin flag) and their graph node. Work that runs on
// someone's behalf without a request (a rule, a scheduled message, a
// reminder, an agent's executor) loads its actor here, so a departed person
// never acts.
func ActorByUUID(ctx context.Context, id uuid.UUID) (*models.UserInfo, error) {
	pg, err := GetActiveUserWithAdminFlagByUserUUID(ctx, id)
	// The read keeps active people only, so no row is someone who has left.
	if pg == nil && (err == nil || errors.Is(err, sql.ErrNoRows)) {
		return nil, ErrNoActor
	}
	if err != nil {
		return nil, err
	}
	dg, err := GetDgraphUserInfoByUUID(ctx, id.String())
	if err != nil {
		return nil, err
	}
	if dg == nil {
		return nil, ErrNoActor
	}
	return &models.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *dg}, nil
}
