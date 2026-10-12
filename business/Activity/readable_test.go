package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

const (
	openDoc     = "dddddddd-0000-4000-8000-000000000001"
	closedDoc   = "dddddddd-0000-4000-8000-000000000002"
	openBoard   = "bbbbbbbb-0000-4000-8000-000000000001"
	closedBoard = "bbbbbbbb-0000-4000-8000-000000000002"
)

func withReadChecks(t *testing.T, docs, boards ReadCheck) {
	t.Helper()
	oldDocs, oldBoards := readableDocs, readableBoards
	readableDocs, readableBoards = docs, boards
	t.Cleanup(func() { readableDocs, readableBoards = oldDocs, oldBoards })
}

func onDoc(doc string) *dgraphStruct.DgraphComment {
	return &dgraphStruct.DgraphComment{Text: "on " + doc, Doc: &dgraphStruct.DgraphDoc{Uuid: doc, Title: "Salaries"}}
}

func onBoard(board string) *dgraphStruct.DgraphComment {
	return &dgraphStruct.DgraphComment{Text: "on " + board, Board: &dgraphStruct.DgraphBoard{Uuid: board, Title: "Layoffs"}}
}

func mentionIn(c *dgraphStruct.DgraphComment) dgraphModels.UnifiedActivityItem {
	return dgraphModels.UnifiedActivityItem{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Comment: c}}
}

// Someone named in a comment on a private doc they can't read, or taken off
// the doc later, found the comment's text and the doc's current title in
// their feed: the feed held a comment on a doc or a board to its deletion
// alone. An item about a doc or a board is now kept only if it says the reader
// may read it now, each asked once a page.
func TestFeedLeavesOutCommentsOnDocsAndBoardsTheReaderCannotRead(t *testing.T) {
	var docCalls, boardCalls int
	var askedDocs, askedBoards []string
	var askedFor string
	withReadChecks(t,
		func(_ context.Context, ids []string, userUUID string) (map[string]bool, error) {
			docCalls++
			askedDocs, askedFor = ids, userUUID
			return map[string]bool{openDoc: true, closedDoc: false}, nil
		},
		func(_ context.Context, ids []string, userUUID string) (map[string]bool, error) {
			boardCalls++
			askedBoards = ids
			return map[string]bool{openBoard: true, closedBoard: false}, nil
		})
	post := dgraphModels.UnifiedActivityItem{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{Uuid: "p1"}}}
	items := []dgraphModels.UnifiedActivityItem{
		mentionIn(onDoc(closedDoc)),
		mentionIn(onDoc(openDoc)),
		mentionIn(onDoc(strings.ToUpper(closedDoc))),
		mentionIn(onBoard(closedBoard)),
		mentionIn(onBoard(openBoard)),
		{ActivityType: "COMMENT", Comment: onDoc(closedDoc)},
		{ActivityType: "REACTION", Reaction: &dgraphModels.ReactionsActivity{Comment: onDoc(closedDoc)}},
		post,
	}
	got := keepReadable(context.Background(), "reader", items, itemAbout)
	want := []dgraphModels.UnifiedActivityItem{items[1], items[4], items[7]}
	if len(got) != len(want) {
		t.Fatalf("kept %d items, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Mention != want[i].Mention {
			t.Errorf("item %d: kept %+v, want %+v", i, got[i], want[i])
		}
	}
	if docCalls != 1 || boardCalls != 1 || askedFor != "reader" || len(askedDocs) != 2 || len(askedBoards) != 2 {
		t.Errorf("asked %d times about docs %v for %q and %d times about boards %v; want once each, about each once, for the reader",
			docCalls, askedDocs, askedFor, boardCalls, askedBoards)
	}

	// The mentions tab reads mentions on their own, held to the same rule.
	mentions := keepReadable(context.Background(), "reader",
		[]*dgraphStruct.DgraphMentions{items[0].Mention, items[1].Mention, post.Mention}, mentionAbout)
	if len(mentions) != 2 || mentions[0] != items[1].Mention || mentions[1] != post.Mention {
		t.Errorf("the mentions tab kept %d mentions, want the one on the doc they can read and the post", len(mentions))
	}
}

// A graph that can't be asked, or a check never handed over, keeps no item
// about a doc or a board, and every other item.
func TestFeedLeavesOutDocsAndBoardsWhenTheyCannotBeAsked(t *testing.T) {
	down := func(context.Context, []string, string) (map[string]bool, error) { return nil, errors.New("graph down") }
	post := dgraphModels.UnifiedActivityItem{ActivityType: "MENTION", Mention: &dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{Uuid: "p1"}}}
	for name, checks := range map[string][2]ReadCheck{"graph down": {down, down}, "never handed over": {nil, nil}} {
		withReadChecks(t, checks[0], checks[1])
		got := keepReadable(context.Background(), "reader",
			[]dgraphModels.UnifiedActivityItem{mentionIn(onDoc(openDoc)), mentionIn(onBoard(openBoard)), post}, itemAbout)
		if len(got) != 1 || got[0].Mention != post.Mention {
			t.Errorf("%s: kept %d items, want only the post", name, len(got))
		}
	}
}
