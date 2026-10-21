package taskrank

import (
	"errors"
	"fmt"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

const review = "11111111-2222-3333-4444-555555555555"

func board(n int) map[string]*dgraphStruct.BoardPlaces {
	todo := &dgraphStruct.BoardPlaces{}
	for i := 0; i < n; i++ {
		r := float64(n - i)
		t := &dgraphStruct.DgraphTask{Uuid: fmt.Sprintf("t%03d", i), Rank: &r}
		if i%4 == 0 {
			s := review
			t.CustomStatus = &s
		}
		todo.Places = append(todo.Places, t)
	}
	todo.Count = n
	return map[string]*dgraphStruct.BoardPlaces{"todo": todo, "done": {Count: 3}}
}

func partOf(t *dgraphStruct.DgraphTask) string {
	if t.CustomStatus != nil {
		return *t.CustomStatus
	}
	return ""
}

// Paging a column after its first page walks it in the board's order, every
// card once, and the first page is the board's own first answer for it.
func TestAColumnPagesInTheBoardsOrderEveryCardOnce(t *testing.T) {
	for _, status := range []string{dgraphStruct.TASK_STATUS_TODO, review} {
		cols := board(250)
		first, _ := Pick(cols, 60, partOf)
		var want []string
		for _, p := range first["todo"] {
			if partOf(p) == status || (partOf(p) == "" && status == dgraphStruct.TASK_STATUS_TODO) {
				want = append(want, p.Uuid)
			}
		}
		var got []string
		after, pages := "", 0
		for {
			page, total, more, err := ColumnPage(board(250), status, partOf, after, 60)
			if err != nil {
				t.Fatalf("%s page %d: %v", status, pages, err)
			}
			if pages == 0 {
				for i, p := range page {
					if i < len(want) && p.Uuid != want[i] {
						t.Fatalf("%s: the first page differs from the board's at %d: %s vs %s", status, i, p.Uuid, want[i])
					}
				}
			}
			for _, p := range page {
				got = append(got, p.Uuid)
			}
			pages++
			if !more {
				if len(got) != total {
					t.Fatalf("%s: walked %d of %d", status, len(got), total)
				}
				break
			}
			after = page[len(page)-1].Uuid
		}
		seen := map[string]bool{}
		for _, id := range got {
			if seen[id] {
				t.Fatalf("%s: %s twice", status, id)
			}
			seen[id] = true
		}
	}
}

func TestAColumnPageSaysWhyItCant(t *testing.T) {
	if _, _, _, err := ColumnPage(board(10), dgraphStruct.TASK_STATUS_TODO, partOf, "gone", 5); !errors.Is(err, ErrNoSuchCard) {
		t.Errorf("after a card that isn't there: %v", err)
	}
	if _, _, _, err := ColumnPage(board(10), dgraphStruct.TASK_STATUS_DONE, partOf, "", 5); !errors.Is(err, ErrClosedColumn) {
		t.Errorf("a closed column: %v", err)
	}
	if _, _, _, err := ColumnPage(board(10), "nonsense", partOf, "", 5); !errors.Is(err, ErrUnknownColumn) {
		t.Errorf("no such column: %v", err)
	}
}
