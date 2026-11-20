package business

import (
	"context"
	"errors"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	domain "github.com/akashc777/OneCamp/domain/Board"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/services/collab"
)

// A board is handed on the way a doc is (docs C6-2, miro-32): by an admin's
// recovery (business/Access Recover), which records it, and which a departed
// member's handover asks for each of their boards (business/Departed).
func init() { accessBusiness.RegisterHandOn(KindBoard, HandOn) }

// HandOn makes toUserUUID the board's owner, live or in the trash: the owner
// before stays on as a co-owner (a doc's keeps full access), its co-owners
// stay co-owners, and the new owner holds no other role. Whoever's place on
// it moved is authorised afresh on its live canvas, and search grants it as
// the graph now says before this returns. Answers the owner before.
func HandOn(ctx context.Context, boardUUID, toUserUUID string) (string, error) {
	before, err := domain.GetBoardPermissions(ctx, boardUUID)
	if errors.Is(err, helpers.ErrNotFound) || (err == nil && (before == nil || before.Uuid == "")) {
		return "", accessBusiness.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	from, to := "", strings.ToLower(toUserUUID)
	if before.CreatedBy != nil {
		from = strings.ToLower(before.CreatedBy.Uuid)
	}
	if from == to {
		return from, nil
	}
	if err := domain.HandOnBoard(ctx, boardUUID, from, to); err != nil {
		if errors.Is(err, helpers.ErrNotFound) {
			return "", accessBusiness.ErrNoSuchPrincipal
		}
		return "", err
	}
	moved := []string{to}
	if from != "" {
		moved = append(moved, from)
	}
	collab.ReauthBoardLater(boardUUID, moved)
	SettleBoardSearch(ctx, boardUUID)
	return from, nil
}
