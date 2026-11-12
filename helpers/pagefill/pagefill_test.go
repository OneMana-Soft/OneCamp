package pagefill

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
)

// source is a list of 1..n read in pages after a keyset cursor (the last
// number read; 0 is the start), counting its reads and the cursors it was
// given.
type source struct {
	n      int
	afters []int
	failAt int // the read (1-based) that fails, if any
	err    error
}

func (s *source) read(_ context.Context, after, size int) ([]int, int, error) {
	s.afters = append(s.afters, after)
	if s.failAt == len(s.afters) {
		return nil, 0, s.err
	}
	var rows []int
	for v := after + 1; v <= s.n && len(rows) < size; v++ {
		rows = append(rows, v)
	}
	next := after
	if len(rows) > 0 {
		next = rows[len(rows)-1]
	}
	return rows, next, nil
}

// keeper keeps the numbers pass says, as words, recording each page it was
// handed.
type keeper struct {
	pass  func(int) bool
	pages [][]int
	err   error
}

func (k *keeper) keep(_ context.Context, rows []int) ([]string, error) {
	k.pages = append(k.pages, append([]int(nil), rows...))
	if k.err != nil {
		return nil, k.err
	}
	out := []string{}
	for _, v := range rows {
		if k.pass(v) {
			out = append(out, strconv.Itoa(v))
		}
	}
	return out, nil
}

func multipleOf(m int) func(int) bool { return func(v int) bool { return v%m == 0 } }

func fill(t *testing.T, s *source, k *keeper, size, want, maxPages int) []string {
	t.Helper()
	got, err := Fill[int, string, int](context.Background(), s.read, k.keep, size, want, maxPages)
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	return got
}

func is(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: %v, want %v", what, got, want)
	}
}

// A page whose rows are mostly left out is filled from the pages after it,
// read after the cursor each page ends on, and the filter is asked once a
// page with that page's rows.
func TestFillReadsOnPastWhatTheFilterLeavesOut(t *testing.T) {
	s, k := &source{n: 20}, &keeper{pass: multipleOf(3)}
	got := fill(t, s, k, 4, 4, 5)
	is(t, "kept", got, []string{"3", "6", "9", "12"})
	is(t, "cursors read after", s.afters, []int{0, 4, 8})
	is(t, "pages handed to the filter", k.pages, [][]int{{1, 2, 3, 4}, {5, 6, 7, 8}, {9, 10, 11, 12}})
}

// It stops as soon as it has want, and answers no more than want though the
// last page kept more.
func TestFillStopsAtWant(t *testing.T) {
	s, k := &source{n: 20}, &keeper{pass: func(int) bool { return true }}
	is(t, "kept", fill(t, s, k, 5, 3, 5), []string{"1", "2", "3"})
	is(t, "reads", len(s.afters), 1)

	s, k = &source{n: 20}, &keeper{pass: multipleOf(2)}
	is(t, "kept across pages", fill(t, s, k, 4, 3, 5), []string{"2", "4", "6"})
	is(t, "reads across pages", len(s.afters), 2)
}

// It reads at most maxPages pages, and answers what those kept, short.
func TestFillStopsAtTheBound(t *testing.T) {
	s, k := &source{n: 100}, &keeper{pass: func(v int) bool { return v > 50 }}
	is(t, "kept", fill(t, s, k, 5, 5, 3), []string{})
	is(t, "reads", len(s.afters), 3)

	s, k = &source{n: 100}, &keeper{pass: multipleOf(7)}
	is(t, "kept within the bound", fill(t, s, k, 5, 5, 2), []string{"7"})
	is(t, "reads within the bound", len(s.afters), 2)
}

// A short page is the source's end: no read after it. An empty one is too.
func TestFillEndsOnAShortPage(t *testing.T) {
	s, k := &source{n: 7}, &keeper{pass: multipleOf(1)}
	is(t, "kept", fill(t, s, k, 5, 10, 5), []string{"1", "2", "3", "4", "5", "6", "7"})
	is(t, "reads", len(s.afters), 2)

	s, k = &source{n: 10}, &keeper{pass: multipleOf(1)}
	is(t, "kept to the end", len(fill(t, s, k, 5, 20, 5)), 10)
	is(t, "reads to an empty page", len(s.afters), 3)
	is(t, "the empty page went to no filter", len(k.pages), 2)
}

// An error from either side is answered as it is, with no rows, and nothing
// is read after it.
func TestFillPassesAnErrorThrough(t *testing.T) {
	down := errors.New("view log down")
	s, k := &source{n: 20, failAt: 2, err: down}, &keeper{pass: multipleOf(9)}
	got, err := Fill[int, string, int](context.Background(), s.read, k.keep, 4, 4, 5)
	if !errors.Is(err, down) || got != nil {
		t.Errorf("a read that failed on the second page: %v %v, want the read's error and no rows", got, err)
	}
	is(t, "reads", len(s.afters), 2)

	refused := errors.New("graph down")
	s, k = &source{n: 20}, &keeper{pass: multipleOf(1), err: refused}
	got, err = Fill[int, string, int](context.Background(), s.read, k.keep, 4, 4, 5)
	if !errors.Is(err, refused) || got != nil {
		t.Errorf("a filter that failed: %v %v, want the filter's error and no rows", got, err)
	}
	is(t, "reads after the filter failed", len(s.afters), 1)
}

// A context ended between two pages ends the fill with its error.
func TestFillStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &source{n: 20}
	read := func(c context.Context, after, size int) ([]int, int, error) {
		defer cancel()
		return s.read(c, after, size)
	}
	got, err := Fill[int, string, int](ctx, read, (&keeper{pass: multipleOf(9)}).keep, 4, 4, 5)
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Errorf("a context ended after the first page: %v %v, want context.Canceled", got, err)
	}
	is(t, "reads", len(s.afters), 1)
}

// Nothing to read, or nothing wanted, is an empty list, never nil; a size
// below 1 reads want a page and a bound below 1 reads one page.
func TestFillEmptyAndDefaults(t *testing.T) {
	s, k := &source{n: 0}, &keeper{pass: multipleOf(1)}
	got := fill(t, s, k, 5, 5, 5)
	if got == nil || len(got) != 0 {
		t.Errorf("an empty source: %#v, want an empty list", got)
	}
	is(t, "reads of an empty source", len(s.afters), 1)

	s = &source{n: 20}
	got = fill(t, s, k, 5, 0, 5)
	if got == nil || len(got) != 0 {
		t.Errorf("want 0: %#v, want an empty list", got)
	}
	is(t, "reads for want 0", len(s.afters), 0)

	s, k = &source{n: 20}, &keeper{pass: multipleOf(2)}
	is(t, "size 0 reads want a page", fill(t, s, k, 0, 3, 5), []string{"2", "4", "6"})
	is(t, "size 0 pages", k.pages, [][]int{{1, 2, 3}, {4, 5, 6}})

	s, k = &source{n: 20}, &keeper{pass: multipleOf(2)}
	is(t, "a bound of 0 reads one page", fill(t, s, k, 4, 3, 0), []string{"2", "4"})
	is(t, "reads for a bound of 0", len(s.afters), 1)
}
