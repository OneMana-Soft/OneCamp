package business

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
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

// A board is live, for a guest's link and the collaboration service, while
// it's there and not deleted, private or not, and asked as no one in
// particular; a graph that doesn't answer says nothing about it.
func TestABoardIsLiveUntilItIsDeleted(t *testing.T) {
	yes := true
	zero, deleted := time.Time{}, time.Now().Add(-time.Hour)
	old := readBoardsAccess
	t.Cleanup(func() { readBoardsAccess = old })
	readBoardsAccess = func(_ context.Context, ids []string, userUUID string) ([]*dgraphStruct.DgraphBoard, error) {
		if userUUID != "" {
			t.Errorf("asked for %q, want no one", userUUID)
		}
		switch ids[0] {
		case "aaaa-down":
			return nil, errors.New("graph down")
		case "aaaa-gone":
			return nil, nil
		}
		boards := map[string]*dgraphStruct.DgraphBoard{
			"aaaa-live":    {Uuid: "AAAA-live", DeletedAt: &zero},
			"aaaa-private": {Uuid: "aaaa-private", IsPrivate: &yes},
			"aaaa-binned":  {Uuid: "aaaa-binned", DeletedAt: &deleted},
		}
		return []*dgraphStruct.DgraphBoard{boards[ids[0]]}, nil
	}
	for id, want := range map[string]bool{"aaaa-live": true, "aaaa-private": true, "aaaa-binned": false, "aaaa-gone": false} {
		if got, err := Live(context.Background(), id); err != nil || got != want {
			t.Errorf("%s: live %v (%v), want %v", id, got, err, want)
		}
	}
	if got, err := Live(context.Background(), "aaaa-down"); err == nil || got {
		t.Errorf("a graph that didn't answer gave %v, %v; want an error and not live", got, err)
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

// A change that took a board from someone settles search before it answers
// (SettleBoardSearch). A write that failed there was only logged, and the
// entries went on granting the board in counts until the next change or a
// search's repair. It is written again in the background now, from the
// board as it is then.
func TestABoardSettleThatFailsIsWrittenAgainInTheBackground(t *testing.T) {
	oldRead, oldWrite, oldComments, oldRefresh := readBoardSharing, writeBoardSearch, writeBoardCommentSearch, refreshBoardSearch
	t.Cleanup(func() {
		readBoardSharing, writeBoardSearch, writeBoardCommentSearch, refreshBoardSearch = oldRead, oldWrite, oldComments, oldRefresh
	})
	// The rewrite goes on to the comments' writer after it hands its fields
	// over: it ends before the seams go back (cleanups run last first).
	t.Cleanup(func() {
		if busy := helpers.SettleBackground(5 * time.Second); len(busy) > 0 {
			t.Errorf("still running when the seams go back: %v", busy)
		}
	})
	private := true
	readBoardSharing = func(context.Context, string) (*dgraphStruct.DgraphBoard, error) {
		return &dgraphStruct.DgraphBoard{IsPrivate: &private}, nil
	}
	writeBoardCommentSearch = func(context.Context, string, opensearchModels.BoardCommentAccess) (int, error) { return 0, nil }
	refreshBoardSearch = func(context.Context) error { return nil }
	var mu sync.Mutex
	writes := 0
	rewritten := make(chan map[string]interface{}, 1)
	writeBoardSearch = func(_ context.Context, _ string, fields map[string]interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		writes++
		if writes == 1 {
			return errors.New("search is not connected")
		}
		rewritten <- fields
		return nil
	}

	SettleBoardSearch(context.Background(), "b1")
	select {
	case fields := <-rewritten:
		if fields["board_private"] != true {
			t.Errorf("the background write: %v; want the board as it is now, private", fields)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a settle whose write failed was never written again")
	}
}

// A board's settles (SettleBoardSearch for a change that took it from
// someone, settleBoardDelete for its deletion) ran under the request's ctx,
// so a closed tab would have stopped them halfway now that the writes honour
// their ctx. Each runs within boardSettleTimeout of the call, whatever
// becomes of the request.
func TestABoardSettleOutlivesItsRequestWithinItsBound(t *testing.T) {
	oldRead, oldWrite, oldComments, oldRefresh := readBoardSharing, writeBoardSearch, writeBoardCommentSearch, refreshBoardSearch
	oldDelete, oldCascade := deleteBoardSearch, cascadeBoardDelete
	t.Cleanup(func() {
		readBoardSharing, writeBoardSearch, writeBoardCommentSearch, refreshBoardSearch = oldRead, oldWrite, oldComments, oldRefresh
		deleteBoardSearch, cascadeBoardDelete = oldDelete, oldCascade
	})
	// What each write's ctx was at the call: the settle cancels its own when it returns.
	type atCall struct {
		err  error
		left time.Duration
		ok   bool
	}
	var mu sync.Mutex
	seen := map[string]atCall{}
	saw := func(what string, ctx context.Context) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := seen[what]; !ok {
			d, bounded := ctx.Deadline()
			seen[what] = atCall{err: ctx.Err(), left: time.Until(d), ok: bounded}
		}
	}
	readBoardSharing = func(context.Context, string) (*dgraphStruct.DgraphBoard, error) {
		return &dgraphStruct.DgraphBoard{}, nil
	}
	writeBoardSearch = func(ctx context.Context, _ string, _ map[string]interface{}) error {
		saw("the sharing settle", ctx)
		return nil
	}
	writeBoardCommentSearch = func(context.Context, string, opensearchModels.BoardCommentAccess) (int, error) { return 0, nil }
	refreshBoardSearch = func(context.Context) error { return nil }
	deleteBoardSearch = func(ctx context.Context, _ string, _ int64) error {
		saw("the delete settle", ctx)
		return nil
	}
	// The AI index's cascade runs in the background, on its own ctx: only the
	// comments' is the settle's.
	aiAsked := make(chan struct{}, 1)
	cascadeBoardDelete = func(ctx context.Context, _ []string, _ string, _ int64, indices []string, _ string) error {
		if len(indices) == 1 && indices[0] == "comments" {
			saw("the delete settle's cascade", ctx)
		} else {
			aiAsked <- struct{}{}
		}
		return nil
	}
	request, cancel := context.WithCancel(context.Background())
	cancel()

	SettleBoardSearch(request, "b1")
	settleBoardDelete(request, "b1", 1)
	select {
	case <-aiAsked:
	case <-time.After(5 * time.Second):
		t.Fatal("the AI index's cascade never ran")
	}
	for _, what := range []string{"the sharing settle", "the delete settle", "the delete settle's cascade"} {
		c, ran := seen[what]
		if !ran {
			t.Errorf("%s never ran", what)
			continue
		}
		if c.err != nil {
			t.Errorf("%s ran under the request's ctx, gone with it: %v", what, c.err)
		}
		if !c.ok || c.left <= 0 || c.left > boardSettleTimeout {
			t.Errorf("%s had %v left (bounded: %v); want a bound of at most %v", what, c.left, c.ok, boardSettleTimeout)
		}
	}
}
