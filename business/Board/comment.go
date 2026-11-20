package business

// Can comment (miro-32 head B): a board's commenters comment on it, as its
// owners, co-owners and editors do, and do nothing else to it.
//
// The live canvas stays read-only for everyone who may not edit the board
// (collabServer.js, V-230), so a commenter's comment is written by the
// server: CommentOnBoard checks who may comment, then the collaboration
// service's comment op (live/ops/comment.js) writes the board's "comments"
// map with the caller as the author whatever the request says, and refuses
// deleting anyone else's message. Then the server mirrors it (a Postgres
// row, a Dgraph Comment, search and the AI index), as the web mirrors an
// editor's comment through /board/commentMention.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// What a comment request does.
const (
	CommentAdd    = "add"
	CommentReply  = "reply"
	CommentDelete = "delete"
)

// MaxCommentText is the most characters one message holds, and MaxThread the
// most messages one comment's thread holds (the comment op's MAX_THREAD).
const (
	MaxCommentText = 4000
	MaxThread      = 500
)

// maxCommentCoord is how far from the board's middle a pin may sit.
const maxCommentCoord = 1e7

// The comment refusals, in the words the routes answer 403 with.
const (
	ErrCannotComment     Refusal = "Only the board's owners, editors and commenters can comment on it."
	ErrNotYourMessage    Refusal = "You can delete only your own messages."
	ErrMirrorEditorsOnly Refusal = "Only the board's editors keep its comments this way; a commenter's comment is kept by the board itself."
)

// The comment requests the board can't take, in the words (and with the
// status) their route answers them with (helpers.WriteRefusal).
var (
	// ErrCommentInput is a comment request the board can't take: 400.
	ErrCommentInput = &helpers.Refusal{Code: "comment_request", Status: http.StatusBadRequest,
		Msg: "Say what to do (add, reply or delete), the comment's id for a reply or a delete, where a new one goes, and between 1 and 4000 characters of text."}
	// ErrThreadFull is a reply to a comment whose thread holds MaxThread
	// messages already: 409.
	ErrThreadFull = &helpers.Refusal{Code: "comment_thread_full", Status: http.StatusConflict,
		Msg: fmt.Sprintf("This comment's thread is full (%d messages). Start a new comment.", MaxThread)}
	// ErrNoSuchComment is a comment or message the board doesn't hold: 404.
	ErrNoSuchComment = &helpers.Refusal{Code: "comment_gone", Status: http.StatusNotFound, Msg: "That comment isn't on the board any more."}
)

// CanComment is who may comment on a board: add a pin, reply, and delete
// their own messages. Whoever may edit it (its owner, co-owners and
// editors) and its commenters; nobody, once it is deleted. Viewers, and
// everyone who reads a board open to the workspace, read the comments only.
func CanComment(board *dgraphStruct.DgraphBoard, userUUID string) bool {
	return board != nil && !isDeleted(board) && (CanEdit(board, userUUID) || board.HasCommentAccess > 0)
}

// BoardCommentRequest is one comment change as asked. It names no author.
type BoardCommentRequest struct {
	Action    string
	CommentID string
	MessageID string
	X, Y      *float64
	Text      string
	Mentioned []string
}

// commentSeams are where commenting writes, and how it names its author:
// seams for tests.
var commentSeams = struct {
	// name is what the comment's author is called on it: as the board's live
	// canvas names them (POST /boardColab/authorize answers the same, and
	// the collaboration service's authorGuard stamps it on an editor's
	// comments), never an address, so a commenter's pin reads as an editor's.
	name   func(ctx context.Context, pgID uuid.UUID) string
	live   func(ctx context.Context, boardUUID string, c collab.CommentChange, by, name string) (collab.CommentAnswer, error)
	sync   func(ctx context.Context, board *dgraphStruct.DgraphBoard, user *dgraphStruct.DgraphUser, pgID uuid.UUID, commentID, text string, mentioned []string) error
	unsync func(ctx context.Context, boardUUID, commentID string) error
}{
	name: userDomain.NameForGuests,
	// Read at call time, so a swap of collab.CommentBoard (its own seam) holds.
	live: func(ctx context.Context, boardUUID string, c collab.CommentChange, by, name string) (collab.CommentAnswer, error) {
		return collab.CommentBoard(ctx, boardUUID, c, by, name)
	},
	sync:   syncBoardComment,
	unsync: unsyncBoardComment,
}

// commentChange is the change a request asks for, or ErrCommentInput.
func commentChange(in BoardCommentRequest) (collab.CommentChange, error) {
	c := collab.CommentChange{Action: in.Action, CommentID: strings.TrimSpace(in.CommentID), MessageID: strings.TrimSpace(in.MessageID), Text: strings.TrimSpace(in.Text)}
	id := func(s string) bool { return s != "" && len(s) <= 64 }
	text := c.Text != "" && utf8.RuneCountInString(c.Text) <= MaxCommentText
	at := func(v *float64) bool { return v != nil && !math.IsNaN(*v) && math.Abs(*v) <= maxCommentCoord }
	ok := false
	switch in.Action {
	case CommentAdd:
		ok, c.X, c.Y = text && at(in.X) && at(in.Y), in.X, in.Y
	case CommentReply:
		ok = text && id(c.CommentID)
	case CommentDelete:
		ok = id(c.CommentID) && (c.MessageID == "" || id(c.MessageID))
	}
	if !ok {
		return collab.CommentChange{}, ErrCommentInput
	}
	return c, nil
}

// CommentOnBoard makes a comment change on a board as the person user, the
// author of whatever it writes. A board they can't read is
// ErrBoardNotFound; one they may not comment on, ErrCannotComment;
// deleting someone else's message, ErrNotYourMessage; a reply to a full
// thread, ErrThreadFull; a pin or message the
// board doesn't hold, ErrNoSuchComment. The change made, its mirror follows
// (a failed mirror is logged: the comment is on the board). Answers the pin
// as it now is, or that it went.
func CommentOnBoard(ctx context.Context, boardUUID string, user *dgraphStruct.DgraphUser, pgID uuid.UUID, in BoardCommentRequest) (collab.CommentAnswer, error) {
	change, err := commentChange(in)
	if err != nil {
		return collab.CommentAnswer{}, err
	}
	board, err := ReadableBoard(ctx, boardUUID, user.Uid, user.Uuid)
	if err != nil {
		return collab.CommentAnswer{}, err
	}
	if !CanComment(board, user.Uuid) {
		return collab.CommentAnswer{}, ErrCannotComment
	}
	answer, err := commentSeams.live(ctx, board.Uuid, change, user.Uuid, commentSeams.name(ctx, pgID))
	if err := commentRefusal(err); err != nil {
		return collab.CommentAnswer{}, err
	}
	switch {
	case answer.Removed:
		err = commentSeams.unsync(ctx, board.Uuid, change.CommentID)
	case answer.Comment != nil:
		var mentioned []string
		if change.Action != CommentDelete {
			mentioned = in.Mentioned
		}
		err = commentSeams.sync(ctx, board, user, pgID, answer.Comment.ID, threadText(answer.Comment), mentioned)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CommentOnBoard: the comment is on the board, its mirror isn't: %+v", err)
	}
	return answer, nil
}

// CommentAsGuest makes a comment change on a live board as a board link's
// guest (business/Guest AuthorizeBoardGuest checked the link and its role):
// by is who they are on the board (guest-<grant id>) and name their name
// under the guest marker, both from the server's token. A guest mentions
// nobody (they never see the board's people), and their comment has no
// mirror: search and activity are the members'. Refusals as CommentOnBoard's.
func CommentAsGuest(ctx context.Context, boardUUID, by, name string, in BoardCommentRequest) (collab.CommentAnswer, error) {
	change, err := commentChange(in)
	if err != nil {
		return collab.CommentAnswer{}, err
	}
	live, err := Live(ctx, boardUUID)
	if err != nil {
		return collab.CommentAnswer{}, err
	}
	if !live {
		return collab.CommentAnswer{}, ErrBoardNotFound
	}
	answer, err := commentSeams.live(ctx, boardUUID, change, by, name)
	if err := commentRefusal(err); err != nil {
		return collab.CommentAnswer{}, err
	}
	return answer, nil
}

// commentRefusal is a comment op's failure as the routes answer it.
func commentRefusal(err error) error {
	var op *collab.OpError
	switch {
	case errors.As(err, &op) && op.Status == http.StatusForbidden:
		return ErrNotYourMessage
	case errors.As(err, &op) && op.Status == http.StatusConflict:
		return ErrThreadFull
	case errors.Is(err, collab.ErrNoSuchBlock):
		return ErrNoSuchComment
	case errors.Is(err, collab.ErrRefused):
		return ErrCommentInput
	}
	return err
}

// threadText is what a pin's mirror holds: its messages, a line each, as the
// web mirrors an editor's.
func threadText(c *collab.BoardComment) string {
	lines := make([]string, 0, len(c.Messages))
	for _, m := range c.Messages {
		lines = append(lines, m.Body)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
