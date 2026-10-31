package business

// An actor for work that runs without a request, such as an import: the
// person it is for, as Postgres holds them now.

import (
	"context"
	"database/sql"
	"errors"

	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// errNotActive is a person no longer active (removed from the workspace), or
// never one: no work acts as them.
var errNotActive = errors.New("that person isn't active in this workspace")

// ActorFor is the person userID as a tables actor of kind, for work that
// runs without their request, such as an import: their admin flag as it
// stands now, never when an API token asked for the work (a token acts as a
// member, helpers.ActsThroughToken), and no actor for a person no longer
// active. A request's own actor comes from the request, which carries the
// flag already (the controllers' actorFrom).
//
// An import's actor carried the person's id alone (tables T-AT, Notion), so
// an admin's import acted as a member: canView, canManage and the Access
// Resolver's tableDecision read the flag.
func ActorFor(ctx context.Context, userID uuid.UUID, kind rowLedger.ActorKind) (Actor, error) {
	// Their admin flag is an admin_users row for their email; a person
	// removed has no row here.
	u, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (u == nil || u.Id == uuid.Nil)) {
		return Actor{}, errNotActive
	}
	if err != nil {
		return Actor{}, err
	}
	return Actor{UserID: u.Id, IsAdmin: u.IsAdmin && !helpers.ActsThroughToken(ctx), Kind: kind}, nil
}

// ImportActor is the person an import is for (its job's TriggeredBy), as the
// actor of the tables it makes: ActorFor, as an import.
func ImportActor(ctx context.Context, person uuid.UUID) (Actor, error) {
	return ActorFor(ctx, person, rowLedger.ActorImport)
}
