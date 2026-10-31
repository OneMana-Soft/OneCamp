package dgraphquery

import (
	"errors"
	"testing"
)

func TestPageArgs(t *testing.T) {
	if got := (Page{First: 1000, Offset: 2000}).Args(); got != "first: 1000, offset: 2000" {
		t.Fatalf("Args() = %q", got)
	}
}

// list is a sorted list of n results, 0 to n-1, read a window at a time; asked
// records each window read.
func list(n int, asked *[]Page) func(Page) ([]int, error) {
	return func(p Page) ([]int, error) {
		*asked = append(*asked, p)
		var got []int
		for i := p.Offset; i < min(p.Offset+p.First, n); i++ {
			got = append(got, i)
		}
		return got, nil
	}
}

// A list comes back whole and in order around Dgraph's 1,000 and around the
// page, the reading stopping at the first short page; an exact multiple of
// the page reads one empty page to learn it has ended.
func TestReadAllReadsEveryPage(t *testing.T) {
	for _, size := range []int{SortedCap, ReadAllPage} {
		for _, n := range []int{0, 1, 999, 1000, 1001, size - 1, size, size + 1, 10_000} {
			var asked []Page
			got, err := ReadAll(size, list(n, &asked))
			if err != nil {
				t.Fatalf("size %d, n=%d: %v", size, n, err)
			}
			if len(got) != n {
				t.Fatalf("size %d, n=%d: got %d results", size, n, len(got))
			}
			for i, v := range got {
				if v != i {
					t.Fatalf("size %d, n=%d: result %d is %d", size, n, i, v)
				}
			}
			if want := n/size + 1; len(asked) != want {
				t.Errorf("size %d, n=%d: %d reads, want %d", size, n, len(asked), want)
			}
			for i, p := range asked {
				if p.First != size || p.Offset != i*size {
					t.Errorf("size %d, n=%d: read %d asked %+v", size, n, i, p)
				}
			}
		}
	}
	var asked []Page
	if _, err := ReadAll(0, list(1, &asked)); err != nil || asked[0].First != ReadAllPage {
		t.Fatalf("a size below 1 read %+v (%v), want pages of ReadAllPage", asked, err)
	}
}

// ReadAll gives at most ReadAllMax results: a list of exactly that many is
// read whole, one more is too many, and no page reaches past the most.
func TestReadAllStopsAtItsMost(t *testing.T) {
	for _, size := range []int{ReadAllPage, 3000} {
		var asked []Page
		got, err := ReadAll(size, list(ReadAllMax, &asked))
		if err != nil || len(got) != ReadAllMax {
			t.Fatalf("size %d: a list of exactly %d gave %d results (%v)", size, ReadAllMax, len(got), err)
		}
		for _, p := range asked {
			if p.Offset+p.First > ReadAllMax+1 {
				t.Fatalf("size %d: read %+v reaches past the most", size, p)
			}
		}
		if _, err := ReadAll(size, list(ReadAllMax+1, new([]Page))); !errors.Is(err, ErrTooMany) {
			t.Fatalf("size %d: a list of %d: err = %v, want ErrTooMany", size, ReadAllMax+1, err)
		}
	}
}

func TestReadAllStopsOnAnError(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	_, err := ReadAll(10, func(p Page) ([]int, error) {
		calls++
		if p.Offset == 10 {
			return nil, boom
		}
		return make([]int, 10), nil
	})
	if !errors.Is(err, boom) || calls != 2 {
		t.Fatalf("err %v after %d calls, want boom after 2", err, calls)
	}
}

// A block that ignores its offset would be read forever; ReadAll gives up.
func TestReadAllEndsAListThatNeverEnds(t *testing.T) {
	_, err := ReadAll(1000, func(Page) ([]int, error) { return make([]int, 1000), nil })
	if !errors.Is(err, ErrTooMany) {
		t.Fatalf("err = %v, want ErrTooMany", err)
	}
}

func TestTotalOrder(t *testing.T) {
	for in, want := range map[string]string{
		"orderdesc: task_created_at": "orderdesc: task_created_at, orderasc: task_uuid",
		"orderasc: task_uuid":        "orderasc: task_uuid",
		"orderdesc:task_uuid":        "orderdesc:task_uuid",
		"":                           "orderasc: task_uuid",
	} {
		if got := TotalOrder(in, "task_uuid"); got != want {
			t.Errorf("TotalOrder(%q) = %q, want %q", in, got, want)
		}
	}
}

// EachPage hands the windows over in order, never an empty one, and stops at
// its most, saying whether the list goes on past it; an error from each
// stops the reading.
func TestEachPageHandsEachWindowUpToItsMost(t *testing.T) {
	for _, c := range []struct {
		n, most int
		more    bool
	}{{0, 10, false}, {7, 10, false}, {8, 10, false}, {10, 10, false}, {11, 10, true}, {25, 10, true}} {
		var asked []Page
		var got []int
		more, err := EachPage(4, c.most, list(c.n, &asked), func(w []int) error {
			if len(w) == 0 {
				t.Errorf("n=%d: an empty window", c.n)
			}
			got = append(got, w...)
			return nil
		})
		if err != nil || more != c.more || len(got) != min(c.n, c.most) {
			t.Fatalf("n=%d, most %d: %d results, more %v (%v)", c.n, c.most, len(got), more, err)
		}
		for i, v := range got {
			if v != i {
				t.Fatalf("n=%d: result %d is %d", c.n, i, v)
			}
		}
		for _, p := range asked {
			if p.Offset+p.First > c.most+1 {
				t.Errorf("n=%d: read %+v reaches past the most", c.n, p)
			}
		}
	}
	stop := errors.New("stop")
	var asked []Page
	if _, err := EachPage(4, 100, list(20, &asked), func([]int) error { return stop }); !errors.Is(err, stop) || len(asked) != 1 {
		t.Fatalf("each's error stops the reading: %v after %d reads", err, len(asked))
	}
}
