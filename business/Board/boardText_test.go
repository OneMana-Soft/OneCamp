package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Board/scene"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchBoardModels "github.com/akashc777/OneCamp/models/openSearch/Board"
)

// textWorld stands in for search, the digests and the board's sharing, and
// counts the requests made to each.
type textWorld struct {
	perms     *dgraphStruct.DgraphBoard
	permsErr  error
	searchErr error
	digests   map[string]string
	search    []string
	requests  map[string]int
}

func newTextWorld(t *testing.T) *textWorld {
	t.Helper()
	yes := true
	w := &textWorld{digests: map[string]string{}, requests: map[string]int{}, perms: &dgraphStruct.DgraphBoard{
		Uuid: "b1", Title: "Offsite", IsPrivate: &yes,
		CreatedBy:   &dgraphStruct.DgraphUser{Uuid: "maya"},
		ReadingUser: []*dgraphStruct.DgraphUser{{Uuid: "ben"}},
	}}
	savedRead, savedWrite, savedDigests, savedKeep := readBoardSharing, writeBoardTexts, boardTextDigests, keepBoardTextDigests
	t.Cleanup(func() {
		readBoardSharing, writeBoardTexts, boardTextDigests, keepBoardTextDigests = savedRead, savedWrite, savedDigests, savedKeep
	})
	readBoardSharing = func(context.Context, string) (*dgraphStruct.DgraphBoard, error) { return w.perms, w.permsErr }
	writeBoardTexts = func(_ context.Context, texts []openSearchBoardModels.BoardText) error {
		w.requests["search"]++
		if w.searchErr != nil {
			return w.searchErr
		}
		for _, x := range texts {
			w.search = append(w.search, x.Text)
		}
		return nil
	}
	boardTextDigests = func(_ context.Context, ids []string) []string {
		w.requests["digests"]++
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = w.digests[id]
		}
		return out
	}
	keepBoardTextDigests = func(_ context.Context, d map[string]string) error {
		w.requests["keep"]++
		for id, v := range d {
			w.digests[id] = v
		}
		return nil
	}
	return w
}

// stateSaying is a saved board state: a frame and, when words is set, a text
// in it.
func stateSaying(t *testing.T, words string) string {
	t.Helper()
	els := []scene.Element{scene.Frame("f1", "Logistics", 0, 0, 400, 400, 1)}
	if words != "" {
		txt := scene.Text("t1", 10, 10, 300, words, scene.TextStyle{}, 1)
		txt["frameId"] = "f1"
		els = append(els, txt)
	}
	state, _, err := scene.State(9, els, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// emptyState is a saved board state with nothing on it.
func emptyState(t *testing.T) string {
	t.Helper()
	state, _, err := scene.State(9, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// stubBoardAIAccess stands where the AI edition keeps a test's sharing writes
// off its AI index. This edition writes a board's sharing to search only, and
// readable_test.go (shared by both editions) stubs those writes itself, so
// there is nothing more to keep off.
func stubBoardAIAccess(t *testing.T) { t.Helper() }

func TestABoardsTextIsWrittenOnceAndOnlyWhenItChanged(t *testing.T) {
	w := newTextWorld(t)
	ctx := context.Background()
	venue := stateSaying(t, "Book the venue")
	if err := writeBoardTextNow(ctx, "b1", venue); err != nil {
		t.Fatal(err)
	}
	if len(w.search) != 1 || !strings.Contains(w.search[0], "Logistics") || !strings.Contains(w.search[0], "Book the venue") {
		t.Fatalf("search got %q", w.search)
	}
	if w.digests["b1"] == "" {
		t.Fatal("the written text kept no digest")
	}

	// A save that moved things and changed no word writes nothing.
	if err := writeBoardTextNow(ctx, "b1", venue); err != nil || len(w.search) != 1 || w.requests["search"] != 1 {
		t.Fatalf("an unchanged text was written again: %v, %d written, %d requests", err, len(w.search), w.requests["search"])
	}

	// A frame's name is something a board says.
	if err := writeBoardTextNow(ctx, "b1", stateSaying(t, "")); err != nil || w.search[len(w.search)-1] != "Logistics" {
		t.Fatalf("a board with only a frame: %v, search %q", err, w.search)
	}
	// A board that says nothing any more keeps no words in search.
	if err := writeBoardTextNow(ctx, "b1", emptyState(t)); err != nil || w.search[len(w.search)-1] != "" {
		t.Fatalf("an emptied board: %v, search %q", err, w.search)
	}
}

func TestABoardsTextAsksItsSharingFirst(t *testing.T) {
	w := newTextWorld(t)
	ctx := context.Background()
	w.permsErr = errors.New("graph down")
	if err := writeBoardTextNow(ctx, "b1", stateSaying(t, "Book the venue")); err == nil || len(w.search) != 0 || len(w.digests) != 0 {
		t.Fatalf("written without the board's sharing: %v, %q", err, w.search)
	}
	w.permsErr = nil
	gone := time.Now().Add(-time.Hour)
	w.perms.DeletedAt = &gone
	if err := writeBoardTextNow(ctx, "b1", stateSaying(t, "Book the venue")); err != nil || len(w.search) != 0 {
		t.Fatalf("a deleted board was written: %v, %q", err, w.search)
	}
	if err := writeBoardTextNow(ctx, "b1", "not base64!"); err == nil {
		t.Fatal("a broken state wasn't refused")
	}
}

// The backfill writes the boards with no text written, a page at a time: each
// page's digests read once, its boards (states and sharing) once, and its text
// written in one request. A request that fails keeps none of its boards'
// digests, and a pass is recorded only when every board was written.
func TestTheBackfillWritesBoardsWithNoTextAPageAtATime(t *testing.T) {
	w := newTextWorld(t)
	ctx := context.Background()
	savedPage, savedBoards, savedDone, savedMark := liveBoardPage, boardsForText, boardTextDone, markBoardTextDone
	t.Cleanup(func() {
		liveBoardPage, boardsForText, boardTextDone, markBoardTextDone = savedPage, savedBoards, savedDone, savedMark
	})
	var all []string
	for i := 0; i < boardTextPage+10; i++ {
		all = append(all, fmt.Sprint("b", i))
	}
	pages := 0
	liveBoardPage = func(_ context.Context, offset, first int) ([]string, error) {
		pages++
		return all[min(offset, len(all)):min(offset+first, len(all))], nil
	}
	unread := map[string]bool{"b1": true}
	var asked [][]string
	boardsForText = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphBoard, error) {
		asked = append(asked, ids)
		var out []*dgraphStruct.DgraphBoard
		for _, id := range ids {
			b := &dgraphStruct.DgraphBoard{Uuid: id, Title: "Board " + id, State: stateSaying(t, "Plan "+id)}
			if unread[id] {
				b.State, b.StateKey = "", "boardState/"+id+".gz"
			}
			out = append(out, b)
		}
		return out, nil
	}
	boardTextDone = func(string) (bool, error) { return false, nil }
	marked := 0
	markBoardTextDone = func() error { marked++; return nil }
	w.digests["b0"] = "written already"

	// Pass 1: b0 is written already, and b1's state can't be read.
	if err := BackfillBoardText(ctx); err != nil {
		t.Fatal(err)
	}
	if pages != 2 || len(asked) != 2 || len(asked[0]) != boardTextPage-1 || asked[0][0] != "b1" || len(asked[1]) != 10 || marked != 0 {
		t.Fatalf("pass 1: %d pages, asked for %d reads, marked %d", pages, len(asked), marked)
	}
	if r := w.requests; r["digests"] != 4 || r["search"] != 2 || r["keep"] != 2 || len(w.search) != len(all)-2 {
		t.Fatalf("pass 1 requests %v, %d written to search", r, len(w.search))
	}
	if !strings.Contains(w.search[0], "Plan b2") || w.digests["b2"] == "" || w.digests["b1"] != "" {
		t.Fatalf("pass 1 wrote %q", w.search[0])
	}

	// Pass 2: b1 reads, and search fails: b1 keeps no digest, and the pass isn't recorded.
	unread, asked = map[string]bool{}, nil
	w.searchErr = errors.New("search down")
	if err := BackfillBoardText(ctx); err != nil || len(asked) != 1 || len(asked[0]) != 1 || w.digests["b1"] != "" || marked != 0 {
		t.Fatalf("pass 2: %v, asked %v, marked %d", err, asked, marked)
	}

	// Pass 3 writes b1 alone, and records the pass.
	w.searchErr = nil
	if err := BackfillBoardText(ctx); err != nil || w.digests["b1"] == "" || len(w.search) != len(all)-1 || marked != 1 {
		t.Fatalf("pass 3: %v, %d written, marked %d", err, len(w.search), marked)
	}
	boardTextDone = func(string) (bool, error) { return true, nil }
	if err := BackfillBoardText(ctx); err != nil || len(w.search) != len(all)-1 {
		t.Fatalf("a recorded pass ran again: %v, %d", err, len(w.search))
	}
}
