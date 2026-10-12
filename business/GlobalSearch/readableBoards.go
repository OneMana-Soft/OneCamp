package business

// Search hits on a board are kept only if the person can read the board now.
//
// A board comment's entry carries a copy of its board's privacy and sharing,
// written once when the comment was written, so a comment on a board that was
// public stayed findable by everyone after the board was made private, and by
// someone after they were taken off it; a deleted board's comments stayed
// findable when the delete's cascade missed them. The board decides, as a doc
// does for its hits (readableDocs.go): a hit on a board, or on a comment on
// one, is kept only if the board's own read rule (business/Board CanRead,
// which also refuses a deleted board) says the person may read it, asked of
// the graph once a page. A page that can't be checked keeps none of them.

import (
	"context"
	"strings"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// boardOfHit is the board a hit is about: a board, or the board a comment is
// on; "" for anything else.
func boardOfHit(h *openSearchStruct.GlobalSearchOpenSearchResp) string {
	switch {
	case h == nil:
		return ""
	case h.Board != nil:
		return h.Board.Uuid
	case h.Comment != nil:
		return h.Comment.CommentBoardUuid
	}
	return ""
}

// readableBoards answers which of the boards the person may read. A seam for
// tests.
var readableBoards = boardBusiness.ReadableBoardsFor

// keepReadableBoardHits returns hits without those on a board the person (by
// uuid) can't read now, nor a comment on one. Hits on anything else are kept
// as they are.
func keepReadableBoardHits(ctx context.Context, userUUID string, hits []*openSearchStruct.GlobalSearchOpenSearchResp) []*openSearchStruct.GlobalSearchOpenSearchResp {
	var boards []string
	seen := map[string]bool{}
	for _, h := range hits {
		if b := strings.ToLower(boardOfHit(h)); b != "" && !seen[b] {
			seen[b] = true
			boards = append(boards, b)
		}
	}
	if len(boards) == 0 {
		return hits
	}
	readable, err := readableBoards(ctx, boards, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "search: could not check %d boards against the graph, leaving their results out: %v", len(boards), err)
		readable = nil
	}
	kept := make([]*openSearchStruct.GlobalSearchOpenSearchResp, 0, len(hits))
	dropped := 0
	for _, h := range hits {
		if b := strings.ToLower(boardOfHit(h)); b != "" && !readable[b] {
			dropped++
			continue
		}
		kept = append(kept, h)
	}
	if dropped > 0 {
		helpers.LogInfoWithContext(ctx, "search: left out %d results on boards the person can't read", dropped)
	}
	return kept
}
