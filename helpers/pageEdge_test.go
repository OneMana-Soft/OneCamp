package helpers

import (
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestAPageEdgeIsTheMessageItStartsNextTo(t *testing.T) {
	at := "2026-10-13T04:05:06.123456789Z"
	id := "4b0c0a43-2f6b-4b3d-9d6e-1f2a3b4c5d6e"
	r := httptest.NewRequest("GET", "/po/oldPosts/c/1760328306?at="+url.QueryEscape(at)+"&id="+id, nil)
	edge, err := PageEdgeOf(r, "1760328306")
	if err != nil {
		t.Fatalf("PageEdgeOf: %v", err)
	}
	want, _ := time.Parse(time.RFC3339Nano, at)
	if !edge.At.Equal(want) || edge.ID != id {
		t.Fatalf("edge = %v %q, want %v %q", edge.At, edge.ID, want, id)
	}
	// Read from the edge's own instant, both ways.
	if !edge.Before().After(want) || !edge.After().Before(want) {
		t.Fatalf("Before %v / After %v don't take in %v", edge.Before(), edge.After(), want)
	}

	// An app from before sends whole seconds, compared as they always were.
	old, err := PageEdgeOf(httptest.NewRequest("GET", "/po/oldPosts/c/1760328306", nil), "1760328306")
	if err != nil || old.ID != "" || !old.Before().Equal(time.Unix(1760328306, 0)) || !old.After().Equal(time.Unix(1760328306, 0)) {
		t.Fatalf("whole seconds: %+v %v", old, err)
	}

	for _, q := range []string{"?at=yesterday&id=" + id, "?at=" + url.QueryEscape(at), "?id=" + id, "?at=" + url.QueryEscape(at) + "&id=not-an-id"} {
		if _, err := PageEdgeOf(httptest.NewRequest("GET", "/x"+q, nil), "1760328306"); err == nil {
			t.Errorf("%s was taken as an edge", q)
		}
	}
}

func TestAPageLeavesItsEdgeOut(t *testing.T) {
	idOf := func(m string) string { return m }
	edge := PageEdge{ID: "e"}

	// The edge and ten others: the ten, and maybe more past them.
	page, more := PageFrom([]string{"e", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}, idOf, edge, 10)
	if len(page) != 10 || page[0] != "1" || !more {
		t.Fatalf("page %v more %v", page, more)
	}
	page, more = PageFrom([]string{"e", "1", "2"}, idOf, edge, 10)
	if len(page) != 2 || more {
		t.Fatalf("page %v more %v", page, more)
	}
	// An edge deleted since: a full page, and more.
	page, more = PageFrom([]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}, idOf, edge, 10)
	if len(page) != 10 || !more {
		t.Fatalf("page %v more %v", page, more)
	}
	// Whole seconds: nothing left out.
	page, more = PageFrom([]string{"e", "1"}, idOf, PageEdge{}, 10)
	if len(page) != 2 || more {
		t.Fatalf("page %v more %v", page, more)
	}
}
