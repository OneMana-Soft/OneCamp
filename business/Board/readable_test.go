package business

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphActivityModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
)

// A board comment's live mention went to everyone it named, the comment's
// text and the board's title with it, where a doc comment's reaches only
// those who may read the doc. It now reaches only someone who may read the
// board, and nobody the graph can't answer for; the board's owner still hears
// of the comment.
func TestABoardMentionReachesOnlyWhoMayReadTheBoard(t *testing.T) {
	yes := true
	oldRead, oldPublish := readBoardsAccess, publishActivity
	t.Cleanup(func() { readBoardsAccess, publishActivity = oldRead, oldPublish })
	readBoardsAccess = func(_ context.Context, ids []string, userUUID string) ([]*dgraphStruct.DgraphBoard, error) {
		if userUUID == "unknowable" {
			return nil, errors.New("graph down")
		}
		b := &dgraphStruct.DgraphBoard{Uuid: ids[0], IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "owner"}}
		if userUUID == "reader" {
			b.HasReadAccess = 1
		}
		return []*dgraphStruct.DgraphBoard{b}, nil
	}
	var told []string
	publishActivity = func(userUUID string, item *dgraphActivityModels.UnifiedActivityItem) {
		told = append(told, userUUID+":"+item.ActivityType)
	}
	board := &dgraphStruct.DgraphBoard{Uuid: "board-1", Title: "Layoffs", CreatedBy: &dgraphStruct.DgraphUser{Uuid: "owner"}}
	mentioned := []*dgraphStruct.DgraphUser{{Uuid: "reader"}, {Uuid: "outsider"}, {Uuid: "unknowable"}}
	publishBoardCommentActivity(&dgraphStruct.DgraphUser{Uuid: "author"}, board, "c1", "the shortlist", time.Now(), mentioned,
		map[string]bool{"reader": true, "outsider": true, "unknowable": true})
	sort.Strings(told)
	if got, want := strings.Join(told, ","), "owner:COMMENT,reader:MENTION"; got != want {
		t.Errorf("told %s, want %s", got, want)
	}
}

// The board rule global search asks: anyone for a board that isn't private,
// its creator and the people it's shared with for one that is, and nobody for
// a deleted board. A live board carries the zero time as its deletion time.
func TestBoardReadRule(t *testing.T) {
	yes, no := true, false
	zero, deleted := time.Time{}, time.Now().Add(-time.Hour)
	owner := &dgraphStruct.DgraphUser{Uuid: "owner"}
	cases := []struct {
		name  string
		board *dgraphStruct.DgraphBoard
		user  string
		want  bool
	}{
		{"no board", nil, "me", false},
		{"public", &dgraphStruct.DgraphBoard{IsPrivate: &no, CreatedBy: owner}, "me", true},
		{"public, never written private", &dgraphStruct.DgraphBoard{CreatedBy: owner}, "me", true},
		{"public, live with the zero deletion time", &dgraphStruct.DgraphBoard{IsPrivate: &no, DeletedAt: &zero}, "me", true},
		{"private, not shared", &dgraphStruct.DgraphBoard{IsPrivate: &yes, CreatedBy: owner}, "me", false},
		{"private, its creator", &dgraphStruct.DgraphBoard{IsPrivate: &yes, CreatedBy: owner}, "owner", true},
		{"private, shared to read", &dgraphStruct.DgraphBoard{IsPrivate: &yes, CreatedBy: owner, HasReadAccess: 1}, "me", true},
		{"private, shared to comment", &dgraphStruct.DgraphBoard{IsPrivate: &yes, CreatedBy: owner, HasCommentAccess: 1}, "me", true},
		{"private, shared to edit", &dgraphStruct.DgraphBoard{IsPrivate: &yes, CreatedBy: owner, HasEditAccess: 1}, "me", true},
		{"public, deleted", &dgraphStruct.DgraphBoard{IsPrivate: &no, CreatedBy: owner, DeletedAt: &deleted}, "me", false},
		{"private, deleted, its creator", &dgraphStruct.DgraphBoard{IsPrivate: &yes, CreatedBy: owner, DeletedAt: &deleted}, "owner", false},
	}
	for _, c := range cases {
		if got := CanRead(c.board, c.user); got != c.want {
			t.Errorf("%s: CanRead = %v, want %v", c.name, got, c.want)
		}
	}
}

// ReadableBoardsFor reads every board once, for the person, with its deletion,
// and answers by the rule above, keyed by lower-case uuid; a board the graph
// doesn't have is left out, and a graph that doesn't answer is an error.
func TestReadableBoardsFor(t *testing.T) {
	yes, no := true, false
	deleted := time.Now().Add(-time.Hour)
	var asked []string
	var askedFor string
	old := readBoardsAccess
	t.Cleanup(func() { readBoardsAccess = old })
	readBoardsAccess = func(_ context.Context, ids []string, userUUID string) ([]*dgraphStruct.DgraphBoard, error) {
		asked, askedFor = ids, userUUID
		return []*dgraphStruct.DgraphBoard{
			{Uuid: "AAAA-open", IsPrivate: &no},
			{Uuid: "aaaa-private", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "owner"}},
			{Uuid: "aaaa-shared", IsPrivate: &yes, HasReadAccess: 1},
			{Uuid: "aaaa-binned", IsPrivate: &no, DeletedAt: &deleted},
		}, nil
	}
	got, err := ReadableBoardsFor(context.Background(), []string{"AAAA-open", "aaaa-private", "aaaa-shared", "aaaa-binned", "aaaa-gone"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"aaaa-open": true, "aaaa-private": false, "aaaa-shared": true, "aaaa-binned": false}
	if len(got) != len(want) {
		t.Fatalf("answered %v, want %v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: readable %v, want %v", id, got[id], w)
		}
	}
	if len(asked) != 5 || askedFor != "me" {
		t.Errorf("asked about %v for %q, want the five boards for the person", asked, askedFor)
	}

	readBoardsAccess = func(context.Context, []string, string) ([]*dgraphStruct.DgraphBoard, error) {
		return nil, errors.New("graph down")
	}
	if got, err := ReadableBoardsFor(context.Background(), []string{"aaaa-open"}, "me"); err == nil || got != nil {
		t.Errorf("a graph that didn't answer gave %v, %v; want an error and no answer", got, err)
	}
}

// Making a board private, or changing who it's shared with, writes the
// board's privacy and sharing onto its comments' search entries, which held
// a copy from when each comment was written.
func TestBoardSharingIsWrittenOntoItsComments(t *testing.T) {
	yes := true
	var gotBoard string
	var got opensearchModels.BoardCommentAccess
	old := writeBoardCommentSearch
	t.Cleanup(func() { writeBoardCommentSearch = old })
	writeBoardCommentSearch = func(_ context.Context, boardUUID string, access opensearchModels.BoardCommentAccess) (int, error) {
		gotBoard, got = boardUUID, access
		return 2, nil
	}
	syncBoardCommentSearch(context.Background(), "board-1", &dgraphStruct.DgraphBoard{
		IsPrivate:      &yes,
		CreatedBy:      &dgraphStruct.DgraphUser{Uuid: "owner"},
		ReadingUser:    []*dgraphStruct.DgraphUser{{Uuid: "reader"}},
		CommentingUser: []*dgraphStruct.DgraphUser{{Uuid: "commenter"}, nil},
	})
	if gotBoard != "board-1" || got.Public || got.CreatedBy != "owner" ||
		len(got.Reading) != 1 || got.Reading[0] != "reader" ||
		len(got.Commenting) != 1 || got.Commenting[0] != "commenter" || len(got.Editing) != 0 {
		t.Errorf("wrote %+v onto %q's comments", got, gotBoard)
	}
}
