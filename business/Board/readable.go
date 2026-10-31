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
	"errors"
	"fmt"
	"strings"
	"time"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	domain "github.com/akashc777/OneCamp/domain/Board"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	openSearchBoardModels "github.com/akashc777/OneCamp/models/openSearch/Board"
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

// Live is whether a board is there and not deleted, asked as no one in
// particular: what a guest's link to it and the collaboration service's read
// of it ask before it opens, since a deleted board opens to nobody. One light
// read, no canvas. An error is the graph not answering, and says nothing
// about the board.
func Live(ctx context.Context, boardUUID string) (bool, error) {
	boards, err := readBoardsAccess(ctx, []string{boardUUID}, "")
	if err != nil {
		return false, err
	}
	for _, b := range boards {
		if b != nil && strings.EqualFold(b.Uuid, boardUUID) {
			return !isDeleted(b), nil
		}
	}
	return false, nil
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

// ReadableBoardTitles names the boards, of those named, the person (by uuid)
// may read now, by CanRead: each one's title, keyed by lower-case uuid. A
// board they may not read, deleted, or not there is left out. One graph query
// for the lot; an error is the graph not answering.
func ReadableBoardTitles(ctx context.Context, boardUUIDs []string, userUUID string) (map[string]string, error) {
	out := make(map[string]string, len(boardUUIDs))
	if len(boardUUIDs) == 0 {
		return out, nil
	}
	boards, err := readBoardsAccess(ctx, boardUUIDs, userUUID)
	if err != nil {
		return nil, err
	}
	for _, b := range boards {
		if b != nil && b.Uuid != "" && CanRead(b, userUUID) {
			out[strings.ToLower(b.Uuid)] = b.Title
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
		if perms, err = readBoardSharing(ctx, boardUUID); err != nil || perms == nil {
			helpers.LogErrorWithContext(ctx, "business/syncBoardCommentSearch could not read board %s's sharing: %v", boardUUID, err)
			return
		}
	}
	if err := writeBoardCommentsNow(ctx, boardUUID, perms); err != nil {
		helpers.LogErrorWithContext(ctx, "business/syncBoardCommentSearch %s: %v", boardUUID, err)
	}
}

// writeBoardCommentsNow writes the board's privacy and sharing, as perms holds
// them, onto its comments' search entries, and says if that failed.
func writeBoardCommentsNow(ctx context.Context, boardUUID string, perms *dgraphStruct.DgraphBoard) error {
	access := opensearchModels.BoardCommentAccess{
		Public:     perms.IsPrivate == nil || !*perms.IsPrivate,
		Reading:    boardUserUUIDs(perms.ReadingUser),
		Editing:    boardUserUUIDs(perms.EditingUser),
		Commenting: boardUserUUIDs(perms.CommentingUser),
	}
	if perms.CreatedBy != nil {
		access.CreatedBy = perms.CreatedBy.Uuid
	}
	n, err := writeBoardCommentSearch(ctx, boardUUID, access)
	if err != nil {
		return fmt.Errorf("could not write the board's sharing onto its comments' search entries: %w", err)
	}
	if n > 0 {
		helpers.LogInfoWithContext(ctx, "business/syncBoardCommentSearch wrote board %s's sharing onto %d comment entries", boardUUID, n)
	}
	return nil
}

// SyncBoardSearch writes the board's privacy and sharing, as the graph holds
// them now, onto its own search entry and its comments' entries: after a
// change of who may read it, and when search finds an entry that disagrees
// with the board (business/GlobalSearch's repair). A write that fails is
// logged; the next change, or search's repair, writes it again.
func SyncBoardSearch(ctx context.Context, boardUUID string) {
	if err := writeBoardSearchNow(ctx, boardUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "business/SyncBoardSearch %s: %v", boardUUID, err)
	}
}

// writeBoardSearchNow is SyncBoardSearch's writes, and what failed of them.
// The comments' entries are written even when the board's own isn't.
func writeBoardSearchNow(ctx context.Context, boardUUID string) error {
	perms, err := readBoardSharing(ctx, boardUUID)
	if err != nil || perms == nil {
		return fmt.Errorf("could not read the board's sharing: %v", err)
	}
	err = writeBoardSearch(ctx, boardUUID, map[string]interface{}{
		"board_private":          perms.IsPrivate != nil && *perms.IsPrivate,
		"board_reading_users":    boardUserUUIDs(perms.ReadingUser),
		"board_editing_users":    boardUserUUIDs(perms.EditingUser),
		"board_commenting_users": boardUserUUIDs(perms.CommentingUser),
	})
	if err != nil {
		err = fmt.Errorf("could not write the board's search entry: %w", err)
	}
	return errors.Join(err, writeBoardCommentsNow(ctx, boardUUID, perms))
}

// readBoardSharing is domain.GetBoardPermissions. A seam for tests.
var readBoardSharing = domain.GetBoardPermissions

// writeBoardSearch is openSearchBoardModels.UpdateBoardFieldsInOpenSearch. A
// seam for tests.
var writeBoardSearch = openSearchBoardModels.UpdateBoardFieldsInOpenSearch

// boardSettleTimeout bounds how long a change that took a board from someone
// waits for search to stop granting it; past it the background write takes
// over, and every page's live check still keeps the board off. The request
// going away (a closed tab) doesn't stop a settle: only this bound does.
const boardSettleTimeout = 10 * time.Second

// boardRewriteTimeout bounds that background write, as domain/Doc bounds a
// doc's.
const boardRewriteTimeout = 2 * time.Minute

// SettleBoardSearch is SyncBoardSearch for a change that took the board from
// someone (unshared, made private): it returns once the next search sees the
// board's entry and its comments' as the board is now, so no count grants it
// (docs C2-1). A write that fails or runs out of time is logged and written
// again in the background, from the board as it is then.
func SettleBoardSearch(ctx context.Context, boardUUID string) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), boardSettleTimeout)
	defer cancel()
	if err := writeBoardSearchNow(wctx, boardUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "business/SettleBoardSearch %s: search not settled, written again in the background: %v", boardUUID, err)
		helpers.GoSafeNamed("board.search-settle", func() {
			bctx, cancel := context.WithTimeout(context.Background(), boardRewriteTimeout)
			defer cancel()
			SyncBoardSearch(bctx, boardUUID)
		})
		return
	}
	if err := refreshBoardSearch(wctx); err != nil {
		helpers.LogErrorWithContext(ctx, "business/SettleBoardSearch %s: %v", boardUUID, err)
	}
}

// settleBoardDelete marks a deleted board's entry and its comments' deleted,
// settled, then asks the AI index in the background.
func settleBoardDelete(ctx context.Context, boardUUID string, at int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), boardSettleTimeout)
	defer cancel()
	if err := deleteBoardSearch(ctx, boardUUID, at); err != nil {
		helpers.LogErrorWithContext(ctx, "business/settleBoardDelete %s: %v", boardUUID, err)
	}
	if err := cascadeBoardDelete(ctx, []string{"comment_board_id"}, boardUUID, at, []string{"comments"}, "cascade"); err != nil {
		helpers.LogErrorWithContext(ctx, "business/settleBoardDelete %s comments: %v", boardUUID, err)
	}
	if err := refreshBoardSearch(ctx); err != nil {
		helpers.LogErrorWithContext(ctx, "business/settleBoardDelete %s: %v", boardUUID, err)
	}
	helpers.GoSafeNamed("board.delete-ai-search", func() {
		_ = cascadeBoardDelete(context.Background(), []string{"board_uuid"}, boardUUID, at, []string{"ai_embeddings"}, "cascade")
	})
}

// The board's search writers. Seams for tests.
var (
	refreshBoardSearch = func(ctx context.Context) error {
		return opensearchInit.RefreshIndices(ctx, openSearchStruct.BOARD_INDEX)
	}
	deleteBoardSearch  = openSearchBoardModels.DeleteBoardInOpenSearch
	cascadeBoardDelete = opensearchModels.SyncCascadingDeletionInOpenSearch
)
