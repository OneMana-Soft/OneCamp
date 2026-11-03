package domain

// Seats on a free licence.
//
// A free OneCamp licence covers a set number of people (helpers.SeatLimit,
// stamped at build time); paid licences and builds from source have no limit.
// Every write that makes someone a member takes its seat through WithSeat:
// the member insert (CreateUserWithMethod), adopting an external row
// (AdoptExternalUser) and reactivation (UpdateDeletedTimeToNullByUUID), so
// every way in (invitation, open sign-up, SSO, SCIM, the admin's Activate
// button) meets it. Bots, external identities and guests are not people on
// the licence and never count.

import (
	"context"
	"database/sql"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

const activeMembersQuery = `SELECT count(*) FROM users
	WHERE deleted_at IS NULL AND is_bot = false AND is_external = false`

// seatsLock is the transaction lock every seat-taking write holds from its
// count to its commit.
const seatsLock = "workspace_seats"

// Seams.
var (
	seatLimit           = helpers.MemberSeatLimit
	countActiveMembers  = queryActiveMembers
	isDeactivatedMember = queryIsDeactivatedMember
	addCapped           = softDelete.Bound.Add
)

// SeatUsage is how many people the workspace has and how many its licence
// covers (0 = unlimited).
func SeatUsage(ctx context.Context) (used, limit int, err error) {
	limit = seatLimit()
	used, err = countActiveMembers(ctx)
	return used, limit, err
}

// EnsureSeatAvailable refuses when one more member would exceed the licence.
// It is the early answer a screen gives before any work (the invitation
// form); the seat itself is taken by WithSeat, which counts again under the
// lock. A failed count is not a refusal: the limit must never lock people out
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

// WithSeat runs add, a write that makes one more person a member, while the
// licence has a seat for them. The count and add run in one transaction
// under seatsLock (a softDelete.Bound, as a capped kind's create takes its
// rows), so two joins at the last seat can't both see it free: the second
// counts once the first has committed. add's statements must go through
// pgexec with the ctx they are given, so they join that transaction; inside
// another transaction (pgexec.InTx) the lock is held until that one ends.
// A full workspace answers SeatLimitError and add doesn't run. A failed
// count is not a refusal, as in EnsureSeatAvailable: add then runs without
// the cap (logged). Inside another transaction the failed count has already
// ended that transaction, so add fails with it.
func WithSeat(ctx context.Context, add func(context.Context) error) error {
	limit := seatLimit()
	if limit <= 0 {
		return add(ctx)
	}
	countFailed := false
	seats := softDelete.Bound{Lock: seatsLock, Most: limit, Full: &helpers.SeatLimitError{Limit: limit},
		Count: func(ctx context.Context) (int, error) {
			n, err := countActiveMembers(ctx)
			countFailed = err != nil
			return n, err
		}}
	err := addCapped(seats, ctx, add)
	if countFailed {
		helpers.LogErrorWithContext(ctx, "domain/WithSeat count failed, allowing: %+v", err)
		return add(ctx)
	}
	return err
}

// withSeatForReactivation runs update, a reactivation, taking a seat only
// when it adds a person: the user is deactivated and is a member, not a bot
// or an external identity. A failed check is not a refusal.
func withSeatForReactivation(ctx context.Context, userUUID uuid.UUID, update func(context.Context) error) error {
	if seatLimit() > 0 {
		if adds, err := isDeactivatedMember(ctx, userUUID); err == nil && adds {
			return WithSeat(ctx, update)
		}
	}
	return update(ctx)
}

// queryActiveMembers counts through pgexec, so inside WithSeat's transaction.
func queryActiveMembers(ctx context.Context) (int, error) {
	return pgexec.Scalar[int](ctx, activeMembersQuery)
}

func queryIsDeactivatedMember(ctx context.Context, userUUID uuid.UUID) (bool, error) {
	adds, err := pgexec.Scalar[bool](ctx,
		`SELECT deleted_at IS NOT NULL AND is_bot = false AND is_external = false FROM users WHERE id = $1`,
		userUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return adds, err
}
