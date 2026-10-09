package domain

// Seats on a free licence.
//
// A free OneCamp licence covers a set number of people (helpers.SeatLimit,
// stamped at build time); paid licences and builds from source have no limit.
// The check sits here, at the two inserts that create a member, at adopting an
// external row (AdoptExternalUser) and at reactivation, so every way in
// (invitation, open sign-up, SSO, SCIM, the admin's Activate button) meets it.
// Bots, external identities and guests are not people on the licence and
// never count.

import (
	"context"
	"database/sql"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

const activeMembersQuery = `SELECT count(*) FROM users
	WHERE deleted_at IS NULL AND is_bot = false AND is_external = false`

// Seams.
var (
	seatLimit           = helpers.MemberSeatLimit
	countActiveMembers  = queryActiveMembers
	isDeactivatedMember = queryIsDeactivatedMember
)

// SeatUsage is how many people the workspace has and how many its licence
// covers (0 = unlimited).
func SeatUsage(ctx context.Context) (used, limit int, err error) {
	limit = seatLimit()
	used, err = countActiveMembers(ctx)
	return used, limit, err
}

// EnsureSeatAvailable refuses when one more member would exceed the licence.
// A failed count is not a refusal: the limit must never lock people out
// because a query timed out.
func EnsureSeatAvailable(ctx context.Context) error {
	limit := seatLimit()
	if limit <= 0 {
		return nil
	}
	used, err := countActiveMembers(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/EnsureSeatAvailable count failed, allowing: %+v", err)
		return nil
	}
	if used >= limit {
		return &helpers.SeatLimitError{Limit: limit}
	}
	return nil
}

// ensureSeatForReactivation applies the limit only when reactivating would
// add a person: the user is deactivated and is a member, not a bot or an
// external identity.
func ensureSeatForReactivation(ctx context.Context, userUUID uuid.UUID) error {
	if seatLimit() <= 0 {
		return nil
	}
	adds, err := isDeactivatedMember(ctx, userUUID)
	if err != nil || !adds {
		return nil
	}
	return EnsureSeatAvailable(ctx)
}

func queryActiveMembers(ctx context.Context) (int, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, activeMembersQuery).Scan(&n)
	return n, err
}

func queryIsDeactivatedMember(ctx context.Context, userUUID uuid.UUID) (bool, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var adds bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT deleted_at IS NOT NULL AND is_bot = false AND is_external = false FROM users WHERE id = $1`,
		userUUID).Scan(&adds)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return adds, err
}
