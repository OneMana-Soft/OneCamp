package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Board/scene"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchBoardModels "github.com/akashc777/OneCamp/models/openSearch/Board"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// textWorld stands in for search, the AI, the digests and the board's sharing,
// and counts the requests made to each.
type textWorld struct {
	perms     *dgraphStruct.DgraphBoard
	permsErr  error
	searchErr error
	ai        bool
	digests   map[string]string
	search    []string
	embedded  []string
	access    []ai.BoardAccess
	forgot    int
	requests  map[string]int
}

func newTextWorld(t *testing.T) *textWorld {
	t.Helper()
	yes := true
	w := &textWorld{ai: true, digests: map[string]string{}, requests: map[string]int{}, perms: &dgraphStruct.DgraphBoard{
		Uuid: "b1", Title: "Offsite", IsPrivate: &yes,
		CreatedBy:   &dgraphStruct.DgraphUser{Uuid: "maya"},
		ReadingUser: []*dgraphStruct.DgraphUser{{Uuid: "ben"}},
	}}
	saved := []any{readBoardSharing, writeBoardTexts, embedBoardTexts, forgetBoardTexts, aiOn, boardTextDigests, keepBoardTextDigests}
	t.Cleanup(func() {
		readBoardSharing = saved[0].(func(context.Context, string) (*dgraphStruct.DgraphBoard, error))
		writeBoardTexts = saved[1].(func(context.Context, []openSearchBoardModels.BoardText) error)
		embedBoardTexts = saved[2].(func(context.Context, []ai.BoardContent) error)
		forgetBoardTexts = saved[3].(func(context.Context, []string) error)
		aiOn = saved[4].(func() bool)
		boardTextDigests = saved[5].(func(context.Context, []string) []string)
		keepBoardTextDigests = saved[6].(func(context.Context, map[string]string) error)
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
	embedBoardTexts = func(_ context.Context, boards []ai.BoardContent) error {
		w.requests["ai"]++
		for _, b := range boards {
			w.embedded, w.access = append(w.embedded, b.Text), append(w.access, b.Access)
		}
		return nil
	}
	forgetBoardTexts = func(_ context.Context, ids []string) error { w.requests["forget"]++; w.forgot += len(ids); return nil }
	aiOn = func() bool { return w.ai }
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

// stubBoardAIAccess keeps a test's sharing writes off the AI index, and
// gathers what they would have written.
func stubBoardAIAccess(t *testing.T) func() []ai.BoardAccess {
	t.Helper()
	old := writeBoardAIAccess
	t.Cleanup(func() { writeBoardAIAccess = old })
	var mu sync.Mutex
	var got []ai.BoardAccess
	writeBoardAIAccess = func(_ string, a ai.BoardAccess) { mu.Lock(); got = append(got, a); mu.Unlock() }
	return func() []ai.BoardAccess { mu.Lock(); defer mu.Unlock(); return append([]ai.BoardAccess(nil), got...) }
}

// A change of who may read a board reaches its AI entries (its text's and its
// comments'), as it reaches its search entries.
func TestBoardSharingReachesItsAIEntries(t *testing.T) {
	got := stubBoardAIAccess(t)
	oldRead, oldWrite, oldComments := readBoardSharing, writeBoardSearch, writeBoardCommentSearch
	t.Cleanup(func() { readBoardSharing, writeBoardSearch, writeBoardCommentSearch = oldRead, oldWrite, oldComments })
	no := false
	readBoardSharing = func(context.Context, string) (*dgraphStruct.DgraphBoard, error) {
		return &dgraphStruct.DgraphBoard{IsPrivate: &no, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "maya"},
			EditingUser: []*dgraphStruct.DgraphUser{{Uuid: "ben"}}}, nil
	}
	writeBoardSearch = func(context.Context, string, map[string]interface{}) error { return nil }
	writeBoardCommentSearch = func(context.Context, string, opensearchModels.BoardCommentAccess) (int, error) { return 0, nil }
	if err := writeBoardSearchNow(context.Background(), "b1"); err != nil {
		t.Fatal(err)
	}
	if a := got(); len(a) != 1 || !a[0].Public || a[0].CreatedBy != "maya" || len(a[0].Editing) != 1 || a[0].Editing[0] != "ben" {
		t.Fatalf("the AI entries got %+v", a)
	}
}

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
	if len(w.embedded) != 1 || !strings.HasPrefix(w.embedded[0], "Offsite\n\n") || !strings.Contains(w.embedded[0], "Book the venue") {
		t.Fatalf("the AI got %q", w.embedded)
	}
	if a := w.access[0]; a.Public || a.CreatedBy != "maya" || len(a.Reading) != 1 || a.Reading[0] != "ben" {
		t.Fatalf("the AI entry's access: %+v", a)
	}

	// A save that moved things and changed no word writes nothing.
	if err := writeBoardTextNow(ctx, "b1", venue); err != nil || len(w.search) != 1 || len(w.embedded) != 1 {
		t.Fatalf("an unchanged text was written again: %v, %d, %d", err, len(w.search), len(w.embedded))
	}

	// A renamed board's AI entry carries its new title.
	w.perms.Title = "Retreat"
	if err := writeBoardTextNow(ctx, "b1", venue); err != nil || len(w.embedded) != 2 || !strings.HasPrefix(w.embedded[1], "Retreat\n\n") {
		t.Fatalf("a rename wasn't written: %v, %q", err, w.embedded)
	}

	// A frame's name is something a board says.
	if err := writeBoardTextNow(ctx, "b1", stateSaying(t, "")); err != nil || w.search[len(w.search)-1] != "Logistics" || len(w.embedded) != 3 {
		t.Fatalf("a board with only a frame: %v, search %q, embedded %q", err, w.search, w.embedded)
	}
	// A board that says nothing any more leaves the AI, and search keeps no words.
	if err := writeBoardTextNow(ctx, "b1", emptyState(t)); err != nil {
		t.Fatal(err)
	}
	if w.search[len(w.search)-1] != "" || w.forgot != 1 || len(w.embedded) != 3 {
		t.Fatalf("an emptied board: search %q, forgot %d, embedded %d", w.search, w.forgot, len(w.embedded))
	}
}

func TestABoardsTextAsksItsSharingFirst(t *testing.T) {
	w := newTextWorld(t)
	ctx := context.Background()
	w.permsErr = errors.New("graph down")
	if err := writeBoardTextNow(ctx, "b1", stateSaying(t, "Book the venue")); err == nil || len(w.search)+len(w.embedded) != 0 || len(w.digests) != 0 {
		t.Fatalf("written without the board's sharing: %v, %q, %q", err, w.search, w.embedded)
	}
	w.permsErr = nil
	gone := time.Now().Add(-time.Hour)
	w.perms.DeletedAt = &gone
	if err := writeBoardTextNow(ctx, "b1", stateSaying(t, "Book the venue")); err != nil || len(w.search)+len(w.embedded) != 0 {
		t.Fatalf("a deleted board was written: %v, %q, %q", err, w.search, w.embedded)
	}
	if err := writeBoardTextNow(ctx, "b1", "not base64!"); err == nil {
		t.Fatal("a broken state wasn't refused")
	}
}

// With the AI off, search still gets the text, and turning the AI on writes it
// to the AI at the next save: the digest says which it was written with.
func TestABoardsTextWithTheAIOff(t *testing.T) {
	w := newTextWorld(t)
	ctx := context.Background()
	w.ai = false
	venue := stateSaying(t, "Book the venue")
	if err := writeBoardTextNow(ctx, "b1", venue); err != nil || len(w.search) != 1 || len(w.embedded) != 0 {
		t.Fatalf("AI off: %v, search %q, embedded %q", err, w.search, w.embedded)
	}
	w.ai = true
	if err := writeBoardTextNow(ctx, "b1", venue); err != nil || len(w.embedded) != 1 {
		t.Fatalf("the AI turned on and the text wasn't embedded: %v, %q", err, w.embedded)
	}
}

// The backfill writes the boards with no text written, a page at a time: each
// page's digests read once, its boards (states and sharing) once, and its text
// written in one request of each kind. A request that fails keeps none of its
// boards' digests, and a pass is recorded only when every board was written.
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
	if r := w.requests; r["digests"] != 4 || r["search"] != 2 || r["ai"] != 2 || r["keep"] != 2 || len(w.search) != len(all)-2 || len(w.embedded) != len(all)-2 {
		t.Fatalf("pass 1 requests %v, %d written to search, %d to the AI", r, len(w.search), len(w.embedded))
	}
	if !strings.Contains(w.search[0], "Plan b2") || !strings.HasPrefix(w.embedded[0], "Board b2\n\n") || w.digests["b2"] == "" || w.digests["b1"] != "" {
		t.Fatalf("pass 1 wrote %q, %q", w.search[0], w.embedded[0])
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
