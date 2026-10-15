package business

// A board decides who finds it, and its comments, in search.
//
// A board comment's search entry carries a copy of its board's privacy and
// sharing, written once when the comment is written (indexAndEmbedBoardComment),
// and making the board private or taking someone off it changed only the
// board's own entry: everyone kept finding the comments of a board that had
// been public. Global search now asks the board about every hit on it, or on
// a comment on it (ReadableBoardsFor, below), and a change to the board's
// privacy or sharing rewrites its comments' copies as well
// (syncBoardCommentSearch), so the index's own grant follows the board too.
//
// The activity feed asks the board the same way before it shows a comment on
// one, and a comment's live mention reaches only someone who may read the
// board (readableBy).

import (
	"context"
	"strings"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	domain "github.com/akashc777/OneCamp/domain/Board"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
)

func init() {
	// The feed can't import this package, which imports it, so it is handed
	// the board's read check here (business/Activity readable.go).
	activityBusiness.RegisterBoardReadCheck(ReadableBoardsFor)
}

// readBoardsAccess reads boards as CanRead decides them, deletion included. A
// seam for tests.
var readBoardsAccess = domain.GetBoardsReadAccessFor

// publishActivity is activityBusiness.PublishActivityToUser. A seam for tests.
var publishActivity = activityBusiness.PublishActivityToUser

// readableBy is whether the person (by uuid) may read the board now; not when
// the graph can't say.
func readableBy(ctx context.Context, boardUUID, userUUID string) bool {
	readable, err := ReadableBoardsFor(ctx, []string{boardUUID}, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Board could not check board %s for %s, telling them nothing: %v", boardUUID, userUUID, err)
		return false
	}
	return readable[strings.ToLower(boardUUID)]
}

// ReadableBoardsFor answers, of the named boards, which the person (by uuid)
// may read now, by CanRead, keyed by lower-case uuid: a board that is private
// and not theirs to see, deleted, or not there at all is false (or absent).
// One graph query for the lot. An error is the graph not answering, and says
// nothing about any board.
func ReadableBoardsFor(ctx context.Context, boardUUIDs []string, userUUID string) (map[string]bool, error) {
	out := make(map[string]bool, len(boardUUIDs))
	if len(boardUUIDs) == 0 {
		return out, nil
	}
	boards, err := readBoardsAccess(ctx, boardUUIDs, userUUID)
	if err != nil {
		return nil, err
	}
	for _, b := range boards {
		if b != nil && b.Uuid != "" {
			out[strings.ToLower(b.Uuid)] = CanRead(b, userUUID)
		}
	}
	return out, nil
}

// EditableBy reports whether the person (by uuid) may change the board's
// canvas now, by CanEdit: a board that is deleted, or isn't there, is
// changed by nobody. An error is the graph not answering.
func EditableBy(ctx context.Context, boardUUID, userUUID string) (bool, error) {
	boards, err := readBoardsAccess(ctx, []string{boardUUID}, userUUID)
	if err != nil {
		return false, err
	}
	for _, b := range boards {
		if b != nil && strings.EqualFold(b.Uuid, boardUUID) {
			return CanEdit(b, userUUID), nil
		}
	}
	return false, nil
}

// writeBoardCommentSearch is opensearchModels.SetBoardCommentAccess. A seam
// for tests.
var writeBoardCommentSearch = opensearchModels.SetBoardCommentAccess

// syncBoardCommentSearch writes the board's privacy and sharing as perms holds
// them (GetBoardPermissions; read here when nil) onto its comments' search
// entries. Best-effort: global search asks the board itself about every hit,
// so an entry this misses is still not shown to someone who can't read the
// board.
func syncBoardCommentSearch(ctx context.Context, boardUUID string, perms *dgraphStruct.DgraphBoard) {
	if perms == nil {
		var err error
		if perms, err = domain.GetBoardPermissions(ctx, boardUUID); err != nil || perms == nil {
			helpers.LogErrorWithContext(ctx, "business/syncBoardCommentSearch could not read board %s's sharing: %v", boardUUID, err)
			return
		}
	}
	access := opensearchModels.BoardCommentAccess{
		Public:     perms.IsPrivate == nil || !*perms.IsPrivate,
		Reading:    boardUserUUIDs(perms.ReadingUser),
		Editing:    boardUserUUIDs(perms.EditingUser),
		Commenting: boardUserUUIDs(perms.CommentingUser),
	}
	if perms.CreatedBy != nil {
		access.CreatedBy = perms.CreatedBy.Uuid
	}
	if n, err := writeBoardCommentSearch(ctx, boardUUID, access); err != nil {
		helpers.LogErrorWithContext(ctx, "business/syncBoardCommentSearch could not write board %s's sharing onto its comments' search entries: %v", boardUUID, err)
	} else if n > 0 {
		helpers.LogInfoWithContext(ctx, "business/syncBoardCommentSearch wrote board %s's sharing onto %d comment entries", boardUUID, n)
	}
}
