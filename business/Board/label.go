package business

// Boards carry sensitivity labels (miro-44 head L, business/Label): a board's
// standing for a person is its own access rule (access.go), and its rows go
// under "boards", never naming a private board.

import (
	"context"
	"errors"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	labelBusiness "github.com/akashc777/OneCamp/business/Label"
	"github.com/akashc777/OneCamp/helpers"
)

func init() {
	labelBusiness.RegisterKind(labelBusiness.Kind{Name: KindBoard, Noun: "board", Category: auditBusiness.CategoryBoards, Stand: boardStanding})
}

// labelBoardRead is the board as the person reads it; a seam for tests.
var labelBoardRead = GetBasicBoardByUUID

// boardStanding is who's standing on a board: read and edit by its access
// rule, owned by its creator, and named only when it isn't private.
func boardStanding(ctx context.Context, who labelBusiness.Who, id string) (labelBusiness.Standing, error) {
	b, err := labelBoardRead(ctx, id, who.UserUID)
	if errors.Is(err, helpers.ErrNotFound) || (err == nil && b == nil) {
		return labelBusiness.Standing{}, labelBusiness.ErrItemNotFound
	}
	if err != nil {
		return labelBusiness.Standing{}, err
	}
	name := "a private board"
	if b.IsPrivate != nil && !*b.IsPrivate {
		name = "‘" + b.Title + "’"
	}
	return labelBusiness.Standing{
		Read: CanRead(b, who.UserUUID),
		Edit: CanEdit(b, who.UserUUID),
		Own:  b.CreatedBy != nil && b.CreatedBy.Uuid == who.UserUUID && who.UserUUID != "",
		Name: name,
	}, nil
}
