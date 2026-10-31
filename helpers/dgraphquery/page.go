package dgraphquery

import (
	"errors"
	"fmt"
	"strings"
)

// SortedCap is the most Dgraph returns from a sorted block (orderasc or
// orderdesc) that names no first, or first: 0. It stops there and says
// nothing: root, nested and var blocks alike, while an unsorted block is never
// cut. Measured on dgraph/dgraph:latest on 14 Oct 2026, when a 10,000-task
// board came back with 4,400 cards and a "show all" list with 1,000 of 2,300.
// So every sorted block names its first (models/dgraph's sortedFirst test
// checks the query text), and a reader that needs all of a sorted list reads
// it with ReadAll.
const SortedCap = 1000

// ReadAllPage is how many results ReadAll asks for at a time. Every page is a
// query that sorts the whole list again to find its window, so a list read a
// page at a time costs about its length squared over the page: on dgraph
// v25.4.1, 50,000 tasks took 25.7 s read 1,000 at a time and 4.5 s read 5,000
// at a time (16 Oct 2026). A page names its first, so it may pass SortedCap.
const ReadAllPage = 5000

// ErrTooMany is a ReadAll of a list longer than ReadAllMax.
var ErrTooMany = errors.New("too many results to read in one go")

// ReadAllMax bounds ReadAll at ten pages, a few seconds of sorting: a longer
// list is read a page at a time by whoever asks for it, and a list that never
// ends (or a block that ignores its offset) fails instead of growing until the
// server runs out of memory.
const ReadAllMax = 10 * ReadAllPage

// Page is one window of a sorted block: First results after the first Offset.
type Page struct {
	First  int
	Offset int
}

// Args is the window as block arguments, to follow the order inside the
// block's parentheses: "first: 1000, offset: 2000".
func (p Page) Args() string {
	return fmt.Sprintf("first: %d, offset: %d", p.First, p.Offset)
}

// ReadAll reads all of a sorted list, size results at a time (ReadAllPage
// when size is below 1), as EachPage does. A list longer than ReadAllMax is
// ErrTooMany; one of exactly ReadAllMax is read whole.
func ReadAll[T any](size int, read func(Page) ([]T, error)) ([]T, error) {
	var all []T
	more, err := EachPage(size, ReadAllMax, read, func(got []T) error {
		all = append(all, got...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if more {
		return nil, ErrTooMany
	}
	return all, nil
}

// EachPage reads a sorted list size results at a time (ReadAllPage when size
// is below 1): read gets each window in turn, from the start, and each is
// handed it, until one comes back short or most results have been handed.
// more says the list goes on past most. Run the reads in one snapshot
// (dgraphInit.InSnapshot): a write between two pages moves the offsets, and a
// result is skipped or read twice.
func EachPage[T any](size, most int, read func(Page) ([]T, error), each func([]T) error) (more bool, err error) {
	if size < 1 {
		size = ReadAllPage
	}
	for offset := 0; ; {
		p := Page{First: min(size, most-offset), Offset: offset}
		if p.First < 1 {
			// As many as most, and no short page yet: one more result says the
			// list goes on.
			next, err := read(Page{First: 1, Offset: offset})
			if err != nil {
				return false, err
			}
			return len(next) > 0, nil
		}
		got, err := read(p)
		if err != nil {
			return false, err
		}
		if len(got) > 0 {
			if err := each(got); err != nil {
				return false, err
			}
		}
		if len(got) < p.First {
			return false, nil
		}
		offset += p.First
	}
}

// TotalOrder makes a sort's order total by adding unique, a predicate no two
// results share, as the last key: pages of a list sorted only by a key many
// results share (a priority, a status) may cut a run of equals differently
// each time, and a result is then read twice or never. order is a block's
// order arguments ("orderdesc: task_due_date"); it comes back unchanged when
// it already sorts by unique, and as unique alone when it is empty.
func TotalOrder(order, unique string) string {
	switch {
	case strings.TrimSpace(order) == "":
		return "orderasc: " + unique
	case strings.HasSuffix(strings.ReplaceAll(order, " ", ""), ":"+unique):
		return order
	}
	return order + ", orderasc: " + unique
}
