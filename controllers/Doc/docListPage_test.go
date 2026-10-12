package docController

import (
	"math"
	"net/url"
	"strconv"
	"testing"
)

// A doc list's paging, as asked of the graph: neither the size nor the index
// was bounded, so a huge page read every doc at once and a negative one
// reached the graph's first: argument. And a huge index overflowed the
// offset, index × size, to a negative one.
func TestDocListPagingIsBounded(t *testing.T) {
	for _, c := range []struct{ index, size, wantIndex, wantSize int }{
		{0, 20, 0, 20},
		{3, 100, 3, 100},
		{0, 5000, 0, maxDocListPage},
		{-2, -10, 0, defaultDocListPage},
		{1, 0, 1, defaultDocListPage},
		{math.MaxInt, 20, 49_999, 20},
		{math.MaxInt, 5000, 9_999, maxDocListPage},
		{math.MaxInt, 0, 49_999, defaultDocListPage},
	} {
		i, s := docListPage(c.index, c.size)
		if i != c.wantIndex || s != c.wantSize {
			t.Errorf("page %d of %d: got page %d of %d, want %d of %d", c.index, c.size, i, s, c.wantIndex, c.wantSize)
		}
		if offset := i * s; offset < 0 || offset >= maxDocListOffset {
			t.Errorf("page %d of %d starts at %d, want 0 to %d", c.index, c.size, offset, maxDocListOffset-1)
		}
	}
}

// Who viewed a doc, a page at a time: a huge page index overflowed the offset
// to a negative one, which Postgres refuses, so the list failed instead of
// coming back empty.
func TestDocViewersPagingIsBounded(t *testing.T) {
	huge := strconv.Itoa(math.MaxInt)
	for _, c := range []struct {
		index, size           string
		wantLimit, wantOffset int
	}{
		{"", "", 50, 0},
		{"2", "10", 10, 20},
		{"-1", "500", 50, 0},
		{huge, "", 50, 999_950},
		{huge, "200", 200, 999_800},
		{huge, "7", 7, 999_999},
	} {
		limit, offset := docViewersPage(url.Values{"pageIndex": {c.index}, "pageSize": {c.size}})
		if limit != c.wantLimit || offset != c.wantOffset {
			t.Errorf("page %q of %q: got %d from %d, want %d from %d", c.index, c.size, limit, offset, c.wantLimit, c.wantOffset)
		}
	}
}
