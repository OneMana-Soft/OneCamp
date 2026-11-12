package business

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Deleted boards (UX-TB-H2-BE): who may bring one back, a page of them, and
// a restore's answers, over the graph's seams. The graph's own writes are
// tests/integration's TestDeletedBoardsComeBackAndNeverOneARuleRemoved and
// domain/Board's TestARestoreNeverWritesBackABoardARuleRemoved.

func trashBoard(id, maker string, private bool, deleted time.Time) *dgraphStruct.DgraphBoard {
	return &dgraphStruct.DgraphBoard{Uuid: id, Title: "Retro " + id, IsPrivate: &private, DeletedAt: &deleted, CreatedBy: &dgraphStruct.DgraphUser{Uuid: maker}}
}

// scratchBoard is a board the app made and took away again (DiscardBoard).
func scratchBoard(id, maker string) *dgraphStruct.DgraphBoard {
	b, yes := trashBoard(id, maker, false, time.Now()), true
	b.Scratch = &yes
	return b
}

func TestOnlyItsMakerOrAnAdminMayBringABoardBack(t *testing.T) {
	gone := time.Now()
	for _, c := range []struct {
		name    string
		board   *dgraphStruct.DgraphBoard
		as      string
		isAdmin bool
		want    bool
	}{
		{"its maker", trashBoard("b", "sam", false, gone), "sam", false, true},
		{"its maker, a private board", trashBoard("b", "sam", true, gone), "sam", false, true},
		{"an admin", trashBoard("b", "sam", false, gone), "ada", true, true},
		{"an admin, a private board", trashBoard("b", "sam", true, gone), "ada", true, false},
		{"someone else", trashBoard("b", "sam", false, gone), "lee", false, false},
		{"a board whose maker isn't known", &dgraphStruct.DgraphBoard{Uuid: "b"}, "", false, false},
		{"no board", nil, "sam", true, false},
		{"its maker, a scratch board", scratchBoard("b", "sam"), "sam", false, false},
		{"an admin, a scratch board", scratchBoard("b", "sam"), "ada", true, false},
	} {
		if got := mayRestore(c.board, c.as, c.isAdmin); got != c.want {
			t.Errorf("%s: may restore %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDeletedBoardsComeAPageAtATime(t *testing.T) {
	old := readBoardTrash
	t.Cleanup(func() { readBoardTrash = old })
	gone := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	var asked [2]int
	readBoardTrash = func(_ context.Context, _ string, _ bool, first, offset int) ([]*dgraphStruct.DgraphBoard, error) {
		asked = [2]int{first, offset}
		all := []*dgraphStruct.DgraphBoard{trashBoard("a", "sam", false, gone), trashBoard("b", "sam", false, gone), trashBoard("c", "sam", false, gone)}
		if offset >= len(all) {
			return nil, nil
		}
		return all[offset:min(offset+first, len(all))], nil
	}
	page, err := BoardTrash(context.Background(), "0x1", "sam", false, "", 2)
	if err != nil || len(page.Boards) != 2 || page.Next != "2" || asked != [2]int{3, 0} {
		t.Fatalf("the first page: %+v %v (asked %v)", page, err, asked)
	}
	if page.Boards[0].DeletedAt == nil || !page.Boards[0].DeletedAt.Equal(gone) || page.Boards[0].Owner.UUID != "sam" || !page.Boards[0].Mine {
		t.Errorf("a listed board: %+v", page.Boards[0])
	}
	page, err = BoardTrash(context.Background(), "0x1", "sam", false, page.Next, 2)
	if err != nil || len(page.Boards) != 1 || page.Boards[0].UUID != "c" || page.Next != "" {
		t.Fatalf("the last page: %+v %v", page, err)
	}
	if _, err := BoardTrash(context.Background(), "0x1", "sam", false, "-1", 2); !errors.Is(err, ErrBoardTrashCursor) {
		t.Errorf("a cursor the list never offered: %v", err)
	}
	page, _ = BoardTrash(context.Background(), "0x1", "sam", false, "9", 2)
	if page == nil || page.Boards == nil || len(page.Boards) != 0 {
		t.Errorf("past the end: %+v, want an empty list, never null", page)
	}
}

// Every page's cursor reads on from where that page ended, not from its
// size: five boards two a page are pages at 0, 2 and 4, and the third holds
// the fifth (CR-247 F1, mutant U4: a cursor of the page's size alone reads
// page 2 for ever, and the web's "Load more" with it).
func TestDeletedBoardsReadOnToTheLastPage(t *testing.T) {
	old := readBoardTrash
	t.Cleanup(func() { readBoardTrash = old })
	gone := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	all := []*dgraphStruct.DgraphBoard{trashBoard("a", "sam", false, gone), trashBoard("b", "sam", false, gone),
		trashBoard("c", "sam", false, gone), trashBoard("d", "sam", false, gone), trashBoard("e", "sam", false, gone)}
	readBoardTrash = func(_ context.Context, _ string, _ bool, first, offset int) ([]*dgraphStruct.DgraphBoard, error) {
		if offset >= len(all) {
			return nil, nil
		}
		return all[offset:min(offset+first, len(all))], nil
	}
	var read []string
	cursor, nexts := "", []string{}
	for pages := 0; pages < 4; pages++ {
		page, err := BoardTrash(context.Background(), "0x1", "sam", false, cursor, 2)
		if err != nil {
			t.Fatalf("page %d (cursor %q): %v", pages+1, cursor, err)
		}
		for _, b := range page.Boards {
			read = append(read, b.UUID)
		}
		nexts = append(nexts, page.Next)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if got, want := fmt.Sprint(nexts), fmt.Sprint([]string{"2", "4", ""}); got != want {
		t.Errorf("the pages' cursors are %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(read), fmt.Sprint([]string{"a", "b", "c", "d", "e"}); got != want {
		t.Errorf("the pages hold %s, want %s: each board once, the fifth on page 3", got, want)
	}
}

func TestARestoreAnswersAsTheBoardIs(t *testing.T) {
	oldRead, oldRestore, oldClear, oldUncascade, oldRefresh, oldBus := readTrashedBoard, restoreTrashed, clearBoardSearch, uncascadeBoard, refreshBoardSearch, resourceEventBusiness.Bus
	t.Cleanup(func() {
		readTrashedBoard, restoreTrashed, clearBoardSearch, uncascadeBoard, refreshBoardSearch, resourceEventBusiness.Bus = oldRead, oldRestore, oldClear, oldUncascade, oldRefresh, oldBus
	})
	var got emitted
	resourceEventBusiness.Bus = &got
	var cleared, uncascaded []string
	var uncascadedMu sync.Mutex // the AI index's write runs in the restore's background goroutine
	clearBoardSearch = func(_ context.Context, id string) error { cleared = append(cleared, id); return nil }
	uncascadeBoard = func(_ context.Context, fields []string, id string, _ []string, by string) error {
		uncascadedMu.Lock()
		uncascaded = append(uncascaded, fields[0]+":"+id+":"+by)
		uncascadedMu.Unlock()
		return nil
	}
	refreshBoardSearch = func(context.Context) error { return nil }

	gone := time.Now()
	boards := map[string]*dgraphStruct.DgraphBoard{
		"trashed": trashBoard("trashed", "sam", false, gone),
		"secret":  trashBoard("secret", "sam", true, gone),
		"live":    trashBoard("live", "sam", false, time.Time{}),
	}
	readTrashedBoard = func(_ context.Context, id, _ string) (*dgraphStruct.DgraphBoard, error) {
		if b, ok := boards[id]; ok {
			return b, nil
		}
		return nil, helpers.NotFound("board not found")
	}
	writes := 0
	restoreTrashed = func(_ context.Context, id string) (bool, error) {
		writes++
		b := boards[id]
		if b == nil || !isDeleted(b) {
			return false, nil
		}
		zero := time.Time{}
		b.DeletedAt = &zero
		return true, nil
	}
	ctx := context.Background()

	for _, c := range []struct {
		name, id, as string
		isAdmin      bool
		want         error
	}{
		{"someone else", "trashed", "lee", false, ErrBoardNotFound},
		{"an admin, a private board", "secret", "ada", true, ErrBoardNotFound},
		{"a board that isn't there", "purged", "sam", true, ErrBoardNotFound},
		{"a live board", "live", "sam", false, ErrBoardNotInTrash},
		{"a live board, someone else", "live", "lee", false, ErrBoardNotFound},
	} {
		if _, err := RestoreBoard(ctx, c.id, "0x1", c.as, c.isAdmin); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if writes != 0 || len(got) != 0 || len(cleared) != 0 {
		t.Fatalf("a refused restore wrote %d times, told %d events, cleared %v", writes, len(got), cleared)
	}

	b, err := RestoreBoard(ctx, "trashed", "0x1", "ada", true)
	if err != nil || b.UUID != "trashed" || b.DeletedAt != nil || b.Title != "Retro trashed" {
		t.Fatalf("an admin restoring a board that isn't private: %+v %v", b, err)
	}
	if len(cleared) != 1 || cleared[0] != "trashed" {
		t.Errorf("the board's search entry cleared %v", cleared)
	}
	uncascadedMu.Lock()
	brought := append([]string(nil), uncascaded...)
	uncascadedMu.Unlock()
	if len(brought) < 1 || brought[0] != "comment_board_id:trashed:cascade" {
		t.Errorf("the comments' entries brought back %v: only what the delete's cascade marked", brought)
	}
	if len(got) != 1 || got[0].Kind != resourceEventBusiness.BoardRestored || got[0].ResourceUUID != "trashed" || got[0].Data["private"] != "false" {
		t.Errorf("the restore told %+v, want one board.restored", got)
	}
	if _, err := RestoreBoard(ctx, "trashed", "0x1", "sam", false); !errors.Is(err, ErrBoardNotInTrash) {
		t.Errorf("a board back already: %v, want ErrBoardNotInTrash", err)
	}

	// Removed between the read and the write (a retention rule's delete): 404, and nothing told.
	boards["racing"] = trashBoard("racing", "sam", false, gone)
	restoreTrashed = func(_ context.Context, id string) (bool, error) {
		delete(boards, id)
		return false, nil
	}
	got = nil
	if _, err := RestoreBoard(ctx, "racing", "0x1", "sam", false); !errors.Is(err, ErrBoardNotFound) {
		t.Errorf("a board a rule removed while it was restored: %v, want ErrBoardNotFound", err)
	}
	if len(got) != 0 {
		t.Errorf("a restore that wrote nothing told %+v", got)
	}
}

func TestOneDeletedBoardIsOnlyTheirsToSee(t *testing.T) {
	old := readTrashedBoard
	t.Cleanup(func() { readTrashedBoard = old })
	gone := time.Now()
	boards := map[string]*dgraphStruct.DgraphBoard{"trashed": trashBoard("trashed", "sam", false, gone), "live": trashBoard("live", "sam", false, time.Time{})}
	readTrashedBoard = func(_ context.Context, id, _ string) (*dgraphStruct.DgraphBoard, error) {
		if b, ok := boards[id]; ok {
			return b, nil
		}
		return nil, helpers.NotFound("board not found")
	}
	ctx := context.Background()
	if b, err := TrashedBoardFor(ctx, "trashed", "0x1", "sam", false); err != nil || b.UUID != "trashed" || b.DeletedAt == nil {
		t.Errorf("its maker: %+v %v", b, err)
	}
	for _, c := range []struct{ id, as string }{{"trashed", "lee"}, {"live", "sam"}, {"gone", "sam"}} {
		if _, err := TrashedBoardFor(ctx, c.id, "0x1", c.as, false); !errors.Is(err, ErrBoardNotFound) {
			t.Errorf("%s as %s: %v, want ErrBoardNotFound", c.id, c.as, err)
		}
	}
}
