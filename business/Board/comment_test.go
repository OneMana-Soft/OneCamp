package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

const (
	boardID   = "6b1f0a52-3c1d-4e8b-9f0a-1d2c3b4a5e6f"
	ownerUUID = "u-owner"
	meUUID    = "u-me"
)

// commentBoard is a private board as the person reads it: their role's count
// set ("co", "edit", "comment", "view", "" none, "owner" its maker).
func commentBoard(role string) *dgraphStruct.DgraphBoard {
	yes, zero := true, time.Time{}
	b := &dgraphStruct.DgraphBoard{Uuid: boardID, IsPrivate: &yes, DeletedAt: &zero, CreatedBy: &dgraphStruct.DgraphUser{Uuid: ownerUUID, Uid: "0x1"}}
	switch role {
	case "co":
		b.HasOwnerAccess, b.HasEditAccess = 1, 1
	case "edit":
		b.HasEditAccess = 1
	case "comment":
		b.HasCommentAccess = 1
	case "view":
		b.HasReadAccess = 1
	case "owner":
		b.CreatedBy.Uuid = meUUID
	}
	return b
}

// The Commenter column (miro-32 head B): owners, co-owners, editors and
// commenters comment; viewers, the workspace reading an open board and
// everyone on a deleted board don't.
func TestCanComment(t *testing.T) {
	gone := commentBoard("comment")
	at := time.Now()
	gone.DeletedAt = &at
	open := commentBoard("")
	no := false
	open.IsPrivate = &no
	for _, c := range []struct {
		name  string
		board *dgraphStruct.DgraphBoard
		want  bool
	}{
		{"its owner", commentBoard("owner"), true},
		{"a co-owner", commentBoard("co"), true},
		{"an editor", commentBoard("edit"), true},
		{"a commenter", commentBoard("comment"), true},
		{"a viewer", commentBoard("view"), false},
		{"nobody on it", commentBoard(""), false},
		{"the workspace on a board open to it", open, false},
		{"a commenter on a deleted board", gone, false},
		{"no board", nil, false},
	} {
		if got := CanComment(c.board, meUUID); got != c.want {
			t.Errorf("%s: CanComment = %v, want %v", c.name, got, c.want)
		}
	}
	// A commenter may comment and read, and nothing else.
	cm := commentBoard("comment")
	if !CanRead(cm, meUUID) || CanEdit(cm, meUUID) || CanManage(cm, meUUID) || MayShare(cm, meUUID) || CanDelete(cm, meUUID) || CanPresent(cm, meUUID) || CanProtect(cm, meUUID, false) {
		t.Errorf("a commenter's rights beyond reading and commenting: read %v edit %v manage %v share %v delete %v present %v protect %v",
			CanRead(cm, meUUID), CanEdit(cm, meUUID), CanManage(cm, meUUID), MayShare(cm, meUUID), CanDelete(cm, meUUID), CanPresent(cm, meUUID), CanProtect(cm, meUUID, false))
	}
}

type commentCalls struct {
	live            []collab.CommentChange
	by, name        string
	synced, unsyncd []string
	syncText        string
	mentioned       []string
}

// boardName is what the board's live canvas calls the caller (the name POST
// /boardColab/authorize answers, which the collaboration service's authorGuard
// stamps on an editor's comments), unlike their graph name.
const boardName = "Cam, as the board names her"

// withCommentSeams reads the board as role and records what commenting
// writes; answer and err are the live op's.
func withCommentSeams(t *testing.T, role string, answer collab.CommentAnswer, err error) *commentCalls {
	t.Helper()
	calls := &commentCalls{}
	savedRead, saved := readBasicBoard, commentSeams
	t.Cleanup(func() { readBasicBoard, commentSeams = savedRead, saved })
	readBasicBoard = func(context.Context, string, string) (*dgraphStruct.DgraphBoard, error) {
		return commentBoard(role), nil
	}
	commentSeams.name = func(context.Context, uuid.UUID) string { return boardName }
	commentSeams.live = func(_ context.Context, _ string, c collab.CommentChange, by, name string) (collab.CommentAnswer, error) {
		calls.live, calls.by, calls.name = append(calls.live, c), by, name
		return answer, err
	}
	commentSeams.sync = func(_ context.Context, _ *dgraphStruct.DgraphBoard, _ *dgraphStruct.DgraphUser, _ uuid.UUID, id, text string, mentioned []string) error {
		calls.synced, calls.syncText, calls.mentioned = append(calls.synced, id), text, mentioned
		return nil
	}
	commentSeams.unsync = func(_ context.Context, _ string, id string) error {
		calls.unsyncd = append(calls.unsyncd, id)
		return nil
	}
	return calls
}

func TestCommentOnBoard(t *testing.T) {
	me := &dgraphStruct.DgraphUser{Uid: "0x9", Uuid: meUUID, UserName: "cam"}
	x, y := 10.5, -4.0
	pin := &collab.BoardComment{ID: "c-1", AuthorID: meUUID, Messages: []collab.BoardCommentMessage{{ID: "m-1", AuthorID: meUUID, Body: "Move this"}, {ID: "m-2", AuthorID: meUUID, Body: "left"}}}
	add := BoardCommentRequest{Action: CommentAdd, X: &x, Y: &y, Text: " Move this ", Mentioned: []string{"u-ed"}}

	t.Run("a commenter adds a pin as themselves, and the server mirrors it", func(t *testing.T) {
		calls := withCommentSeams(t, "comment", collab.CommentAnswer{Comment: pin}, nil)
		got, err := CommentOnBoard(context.Background(), boardID, me, uuid.New(), add)
		if err != nil || got.Comment == nil || got.Comment.ID != "c-1" {
			t.Fatalf("answered %+v, %v", got, err)
		}
		if len(calls.live) != 1 || calls.by != meUUID || calls.name != boardName || calls.live[0].Text != "Move this" || *calls.live[0].X != x {
			t.Errorf("the live op was asked %+v by %q named %q, want one add by %q named %q", calls.live, calls.by, calls.name, meUUID, boardName)
		}
		if len(calls.synced) != 1 || calls.synced[0] != "c-1" || calls.syncText != "Move this\nleft" || len(calls.mentioned) != 1 {
			t.Errorf("mirrored %v %q %v, want c-1 with its thread and the mention", calls.synced, calls.syncText, calls.mentioned)
		}
	})
	t.Run("owners, co-owners and editors comment too", func(t *testing.T) {
		for _, role := range []string{"owner", "co", "edit"} {
			calls := withCommentSeams(t, role, collab.CommentAnswer{Comment: pin}, nil)
			if _, err := CommentOnBoard(context.Background(), boardID, me, uuid.New(), add); err != nil || len(calls.live) != 1 {
				t.Errorf("%s: %v, live calls %d", role, err, len(calls.live))
			}
		}
	})
	t.Run("a viewer is refused in words, and nothing is written", func(t *testing.T) {
		calls := withCommentSeams(t, "view", collab.CommentAnswer{Comment: pin}, nil)
		if _, err := CommentOnBoard(context.Background(), boardID, me, uuid.New(), add); !errors.Is(err, ErrCannotComment) || len(calls.live)+len(calls.synced) != 0 {
			t.Errorf("a viewer: %v, live %d, mirrored %d", err, len(calls.live), len(calls.synced))
		}
	})
	t.Run("whoever can't read the board finds no board", func(t *testing.T) {
		calls := withCommentSeams(t, "", collab.CommentAnswer{Comment: pin}, nil)
		if _, err := CommentOnBoard(context.Background(), boardID, me, uuid.New(), add); !errors.Is(err, ErrBoardNotFound) || len(calls.live) != 0 {
			t.Errorf("nobody on it: %v, live %d", err, len(calls.live))
		}
	})
	t.Run("deleting someone else's message is refused in words", func(t *testing.T) {
		calls := withCommentSeams(t, "comment", collab.CommentAnswer{}, &collab.OpError{Op: "comment", Status: 403, Msg: "not yours"})
		_, err := CommentOnBoard(context.Background(), boardID, me, uuid.New(), BoardCommentRequest{Action: CommentDelete, CommentID: "c-1", MessageID: "m-9"})
		if !errors.Is(err, ErrNotYourMessage) || len(calls.unsyncd)+len(calls.synced) != 0 {
			t.Errorf("another's message: %v, mirror %v %v", err, calls.unsyncd, calls.synced)
		}
	})
	t.Run("a pin that's gone is no such comment", func(t *testing.T) {
		withCommentSeams(t, "comment", collab.CommentAnswer{}, &collab.OpError{Op: "comment", Status: 404, Msg: "no such comment"})
		if _, err := CommentOnBoard(context.Background(), boardID, me, uuid.New(), BoardCommentRequest{Action: CommentReply, CommentID: "c-gone", Text: "hi"}); !errors.Is(err, ErrNoSuchComment) {
			t.Errorf("a gone pin: %v", err)
		}
	})
	t.Run("deleting one's own message from a pin that keeps another's mirrors the rest, mentioning nobody", func(t *testing.T) {
		kept := &collab.BoardComment{ID: "c-1", AuthorID: "u-ed", Messages: []collab.BoardCommentMessage{{ID: "m-2", AuthorID: "u-ed", Body: "left"}}}
		calls := withCommentSeams(t, "comment", collab.CommentAnswer{Comment: kept}, nil)
		del := BoardCommentRequest{Action: CommentDelete, CommentID: "c-1", MessageID: "m-1", Mentioned: []string{"u-ed"}}
		if _, err := CommentOnBoard(context.Background(), boardID, me, uuid.New(), del); err != nil || len(calls.synced) != 1 || calls.syncText != "left" || len(calls.mentioned) != 0 {
			t.Errorf("own delete from a kept pin: %v, mirrored %v %q mentioning %v, want c-1 with what's left and no mentions", err, calls.synced, calls.syncText, calls.mentioned)
		}
	})
	t.Run("a reply to a full thread is refused in its own words", func(t *testing.T) {
		calls := withCommentSeams(t, "comment", collab.CommentAnswer{}, &collab.OpError{Op: "comment", Status: 409, Msg: "the thread is full"})
		_, err := CommentOnBoard(context.Background(), boardID, me, uuid.New(), BoardCommentRequest{Action: CommentReply, CommentID: "c-1", Text: "one more"})
		if !errors.Is(err, ErrThreadFull) || !strings.Contains(err.Error(), "500 messages") || len(calls.synced) != 0 {
			t.Errorf("a full thread: %v, mirrored %v, want ErrThreadFull in its words", err, calls.synced)
		}
	})
	t.Run("deleting one's own last message takes the pin and its mirror", func(t *testing.T) {
		calls := withCommentSeams(t, "comment", collab.CommentAnswer{Removed: true}, nil)
		if _, err := CommentOnBoard(context.Background(), boardID, me, uuid.New(), BoardCommentRequest{Action: CommentDelete, CommentID: "c-1", MessageID: "m-1"}); err != nil || len(calls.unsyncd) != 1 || calls.unsyncd[0] != "c-1" {
			t.Errorf("own delete: %v, unmirrored %v", err, calls.unsyncd)
		}
	})
	t.Run("a request the board can't take writes nothing", func(t *testing.T) {
		long := make([]rune, MaxCommentText+1)
		for i := range long {
			long[i] = 'a'
		}
		far := 1e7 + 1
		for name, in := range map[string]BoardCommentRequest{
			"no action":           {X: &x, Y: &y, Text: "hi"},
			"a pin with no spot":  {Action: CommentAdd, Text: "hi"},
			"a pin off the board": {Action: CommentAdd, X: &far, Y: &y, Text: "hi"},
			"no text":             {Action: CommentAdd, X: &x, Y: &y, Text: "  "},
			"too long":            {Action: CommentAdd, X: &x, Y: &y, Text: string(long)},
			"a reply to nothing":  {Action: CommentReply, Text: "hi"},
			"a delete of nothing": {Action: CommentDelete},
		} {
			calls := withCommentSeams(t, "comment", collab.CommentAnswer{Comment: pin}, nil)
			if _, err := CommentOnBoard(context.Background(), boardID, me, uuid.New(), in); !errors.Is(err, ErrCommentInput) || len(calls.live) != 0 {
				t.Errorf("%s: %v, live %d", name, err, len(calls.live))
			}
		}
	})
}

// The web's mirror routes (/board/commentMention, /board/commentDelete) are
// the live canvas's writers', its editors': a commenter's comment is mirrored
// by CommentOnBoard, so a commenter can't rewrite or drop anyone's mirror.
func TestTheWebsMirrorIsTheEditorsOnly(t *testing.T) {
	withCommentSeams(t, "comment", collab.CommentAnswer{}, nil)
	cm := &dgraphStruct.DgraphUser{Uid: "0x9", Uuid: meUUID}
	if err := SyncBoardComment(context.Background(), cm, uuid.New(), boardID, "c-1", "rewritten", nil); !errors.Is(err, ErrMirrorEditorsOnly) {
		t.Errorf("a commenter's mirror write: %v, want ErrMirrorEditorsOnly", err)
	}
	if err := DeleteBoardComment(context.Background(), cm, boardID, "c-1"); !errors.Is(err, ErrMirrorEditorsOnly) {
		t.Errorf("a commenter's mirror delete: %v, want ErrMirrorEditorsOnly", err)
	}
}
