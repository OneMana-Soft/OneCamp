package business

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

const (
	readableBoard   = "bbbbbbbb-0000-4000-8000-000000000001"
	unreadableBoard = "bbbbbbbb-0000-4000-8000-000000000002"
	goneBoard       = "bbbbbbbb-0000-4000-8000-000000000003"
)

func withReadableBoards(t *testing.T, fn func(ctx context.Context, boards []string, userUUID string) (map[string]bool, error)) {
	t.Helper()
	old := readableBoards
	readableBoards = fn
	t.Cleanup(func() { readableBoards = old })
}

func boardHit(id string) *openSearchStruct.GlobalSearchOpenSearchResp {
	return &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.BOARD_TYPE, Board: &openSearchStruct.OpenSearchBoard{Uuid: id}}
}

func commentOnBoard(board string) *openSearchStruct.GlobalSearchOpenSearchResp {
	return &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.COMMENT_TYPE,
		Comment: &openSearchStruct.OpenSearchComment{Uuid: "c-" + board, CommentBoardUuid: board, CommentBoardPublic: true}}
}

// A board made private, or someone taken off one, kept its comments findable:
// each comment's entry holds the board's privacy and sharing from when it was
// written, and global search granted hits on that copy alone. Every hit on a
// board, or on a comment on one, is now kept only if the board says the
// person may read it now.
func TestSearchKeepsOnlyTheBoardsThePersonCanRead(t *testing.T) {
	var askedFor string
	var askedAbout []string
	calls := 0
	withReadableBoards(t, func(_ context.Context, boards []string, userUUID string) (map[string]bool, error) {
		calls++
		askedFor, askedAbout = userUUID, boards
		return map[string]bool{readableBoard: true, unreadableBoard: false}, nil
	})
	chatComment := &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.COMMENT_TYPE, Comment: &openSearchStruct.OpenSearchComment{Uuid: "chat"}}
	hits := []*openSearchStruct.GlobalSearchOpenSearchResp{
		boardHit(readableBoard), boardHit(unreadableBoard), boardHit(goneBoard),
		commentOnBoard(unreadableBoard), commentOnBoard(readableBoard), commentOnBoard(goneBoard),
		docHit(unreadableDoc), task(liveTask), chatComment,
	}
	got := keepReadableBoardHits(context.Background(), "me", hits)
	want := []*openSearchStruct.GlobalSearchOpenSearchResp{hits[0], hits[4], hits[6], hits[7], hits[8]}
	if len(got) != len(want) {
		t.Fatalf("kept %d hits, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hit %d: kept %+v, want %+v", i, got[i], want[i])
		}
	}
	if calls != 1 || askedFor != "me" || len(askedAbout) != 3 {
		t.Errorf("asked %d times about %v for %q, want once about the three boards for the person searching", calls, askedAbout, askedFor)
	}
}

// A graph that can't be asked keeps no board hit, nor a comment on a board,
// and every other hit.
func TestSearchLeavesBoardsOutWhenTheGraphCannotBeAsked(t *testing.T) {
	withReadableBoards(t, func(context.Context, []string, string) (map[string]bool, error) {
		return nil, errors.New("graph down")
	})
	got := keepReadableBoardHits(context.Background(), "me", []*openSearchStruct.GlobalSearchOpenSearchResp{
		boardHit(readableBoard), commentOnBoard(readableBoard), task(liveTask)})
	if len(got) != 1 || got[0].Task == nil {
		t.Fatalf("kept %d hits with the graph down, want only the task", len(got))
	}
}

// The search bar's search puts every page through the board check as well: a
// comment on a board that was public when it was written, and the board, are
// left out for someone the board, now private, isn't shared with.
func TestUnifiedSearchChecksEveryBoardHitWithTheGraph(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"hits":{"hits":[
			{"_index":"comments","_id":"c1","_source":{"comment_id":"c1","comment_body":"layoffs list","comment_board_id":%q,"comment_board_public":true}},
			{"_index":"boards","_id":%q,"_source":{"board_uuid":%q,"board_title":"Layoffs","board_private":false}},
			{"_index":"tasks","_id":%q,"_source":{"task_id":%q,"task_name":"Layoffs comms"}}]}}`,
			unreadableBoard, unreadableBoard, unreadableBoard, liveTask, liveTask)
	}))
	defer node.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{node.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	oldClient := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { opensearchInit.OpenSearchClient = oldClient })
	withChecks(t, map[string]liveness.Checker{}) // nothing gone
	var asked string
	withReadableBoards(t, func(_ context.Context, boards []string, userUUID string) (map[string]bool, error) {
		asked = userUUID
		return map[string]bool{unreadableBoard: false}, nil
	})

	p, err := GetUnifiedGlobalSearch(context.Background(), "me", "me@example.test", nil, nil, nil, "layoffs")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Page) != 1 || p.Page[0].Task == nil {
		t.Fatalf("the page holds %d hits; want only the task, not the board or its comment", len(p.Page))
	}
	if asked != "me" {
		t.Errorf("the board was checked for %q, want the person searching", asked)
	}
}
