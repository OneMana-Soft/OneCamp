package helpers

import (
	"errors"
	"net/url"
	"testing"
)

func TestListPaging(t *testing.T) {
	q := func(s string) url.Values { v, _ := url.ParseQuery(s); return v }
	if _, _, _, err := ListPaging(q("")); !errors.Is(err, ErrListPaging) {
		t.Errorf("no paging: got %v, want ErrListPaging", err)
	}
	if all, _, _, err := ListPaging(q("getAll=true")); err != nil || !all {
		t.Errorf("getAll: got %v %v", all, err)
	}
	if _, size, index, err := ListPaging(q("pageSize=20&pageIndex=2")); err != nil || size != 20 || index != 2 {
		t.Errorf("paged: got %d %d %v", size, index, err)
	}
	for _, bad := range []string{"pageSize=x&pageIndex=0", "pageSize=10&pageIndex=-1"} {
		if _, _, _, err := ListPaging(q(bad)); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestClosedLimit(t *testing.T) {
	q := func(s string) url.Values { v, _ := url.ParseQuery(s); return v }
	for in, want := range map[string]int{"": 200, "closedLimit=x": 200, "closedLimit=50": 200, "closedLimit=400": 400, "closedLimit=99999": 2000} {
		if got := ClosedLimit(q(in)); got != want {
			t.Errorf("%q: got %d, want %d", in, got, want)
		}
	}
}

// A cursor reads back as the offset it was made for, and anything else, a
// cursor someone edited included, reads as the first page: a list never
// starts before its start, or from an error.
func TestOffsetCursor(t *testing.T) {
	for _, n := range []int{0, 1, 30, 12345} {
		if got := CursorOffset(OffsetCursor(n)); got != n {
			t.Errorf("%d read back as %d", n, got)
		}
	}
	for _, junk := range []string{"", "not base64!", OffsetCursor(-5), "eA"} {
		if got := CursorOffset(junk); got != 0 {
			t.Errorf("%q read as %d, want 0", junk, got)
		}
	}
}

// A list that refuses what it didn't hand out tells no cursor (the first page)
// from a cursor it can't read.
func TestReadCursor(t *testing.T) {
	for in, want := range map[string]int{"": 0, OffsetCursor(0): 0, OffsetCursor(30): 30, OffsetCursor(12345): 12345} {
		if got, ok := ReadCursor(in); !ok || got != want {
			t.Errorf("%q: got %d, %v; want %d, true", in, got, ok, want)
		}
	}
	for _, junk := range []string{"not base64!", OffsetCursor(-5), "eA", "MzA="} {
		if got, ok := ReadCursor(junk); ok || got != 0 {
			t.Errorf("%q read as %d, %v; want a refusal", junk, got, ok)
		}
	}
}

func TestLimitParam(t *testing.T) {
	q := func(s string) url.Values { v, _ := url.ParseQuery(s); return v }
	for in, want := range map[string]int{"": 30, "limit=x": 30, "limit=0": 30, "limit=-3": 30, "limit=1": 1, "limit=45": 45, "limit=101": 100} {
		if got := LimitParam(q(in), 30, 100); got != want {
			t.Errorf("%q: got %d, want %d", in, got, want)
		}
	}
}

func TestOpenLimit(t *testing.T) {
	q := func(s string) url.Values { v, _ := url.ParseQuery(s); return v }
	for in, want := range map[string]int{"": 1000, "openLimit=x": 1000, "openLimit=0": 1000, "openLimit=-5": 1000, "openLimit=500": 500, "openLimit=2000": 2000, "openLimit=99999": 5000} {
		if got := OpenLimit(q(in)); got != want {
			t.Errorf("%q: got %d, want %d", in, got, want)
		}
	}
}
