package business

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// The search page said "20 results" for the first 20 of any number, and
// had no way to the rest (SA-BUG-9). A page with more after it carries a
// cursor, and the cursor asks for the hits after the page's last one.
func TestSearchPagesCarryACursorToTheNextPage(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	hits := 21
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var rows []string
		for i := 0; i < hits; i++ {
			// Best first: the score falls, the time with it.
			rows = append(rows, fmt.Sprintf(`{"_index":"channels","_id":"c%d","_source":{"ch_id":"c%d","ch_name":"alpha %d"},"sort":[%d.5,%d]}`,
				i, i, i, 100-i, 1760000000000-int64(i)*1000))
		}
		fmt.Fprintf(w, `{"hits":{"hits":[%s]}}`, strings.Join(rows, ","))
	}))
	defer node.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{node.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	old := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { opensearchInit.OpenSearchClient = old })
	withChecks(t, map[string]liveness.Checker{})

	first, err := GetUnifiedGlobalSearch(context.Background(), "me", "me@example.test", nil, nil, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Page) != 20 || !first.HasMore || first.Next == "" {
		t.Fatalf("a first page of 21 hits: %d hits, has_more %v, next %q; want 20, true and a cursor", len(first.Page), first.HasMore, first.Next)
	}
	after, err := DecodeSearchCursor(first.Next)
	if err != nil {
		t.Fatalf("the page's own cursor doesn't read back: %v", err)
	}
	// The 20th hit's place: score 81.5, time 1760000000000-19000.
	if after[0] != 81.5 || after[1] != 1759999981000 {
		t.Errorf("the cursor holds %v; want the 20th hit's score and time", after)
	}
	if strings.Contains(bodies[0], "search_after") {
		t.Error("the first page asked for hits after something")
	}

	hits = 3
	next, err := GetUnifiedGlobalSearchPage(context.Background(), "me", "me@example.test", nil, nil, nil, "alpha", after, Within{})
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		SearchAfter []float64 `json:"search_after"`
	}
	if err := json.Unmarshal([]byte(bodies[1]), &sent); err != nil {
		t.Fatalf("the second page's request isn't JSON: %v\n%s", err, bodies[1])
	}
	if len(sent.SearchAfter) != 2 || sent.SearchAfter[0] != 81.5 || sent.SearchAfter[1] != 1759999981000 {
		t.Errorf("the second page asked for hits after %v; want after the first page's last", sent.SearchAfter)
	}
	if len(next.Page) != 3 || next.HasMore || next.Next != "" {
		t.Errorf("a last page of 3: %d hits, has_more %v, next %q; want 3, false and no cursor", len(next.Page), next.HasMore, next.Next)
	}
}

// A cursor is two finite numbers this server wrote; anything else is
// refused before it reaches a query.
func TestASearchCursorIsTwoNumbersOrNothing(t *testing.T) {
	good := encodeSearchCursor([]any{12.5, 1760000000000.0})
	if v, err := DecodeSearchCursor(good); err != nil || len(v) != 2 {
		t.Fatalf("a cursor this server wrote reads back as %v, %v", v, err)
	}
	for _, bad := range []string{
		"", "not base64!",
		encodeSearchCursor([]any{1.0}),
		encodeSearchCursor([]any{1.0, 2.0, 3.0}),
		encodeSearchCursor([]any{"1", "x"}),
		encodeSearchCursor([]any{`"}, "script_fields": {`, 2.0}),
	} {
		if _, err := DecodeSearchCursor(bad); err == nil {
			t.Errorf("the cursor %q was taken", bad)
		}
	}
}

// The demo visitor's page says there is more when a person the demo keeps
// from its visitor was among its hits. The people were left out before the
// page was cut, so a page whose 21 hits held one lost its 21st and answered
// has_more false: "Show more" went, and with it every hit after the page.
func TestTheDemoVisitorsPageKeepsShowMore(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var rows []string
		for i := 0; i < 21; i++ {
			sort := fmt.Sprintf(`[%d.5,%d]`, 100-i, 1760000000000-int64(i)*1000)
			if i == 4 {
				// The workspace's owner, whom the demo keeps from its visitor.
				rows = append(rows, fmt.Sprintf(`{"_index":"users","_id":"u%d","_source":{"user_id":"u%d","user_name":"owner","user_email":"owner@example.com"},"sort":%s}`, i, i, sort))
				continue
			}
			rows = append(rows, fmt.Sprintf(`{"_index":"channels","_id":"c%d","_source":{"ch_id":"c%d","ch_name":"alpha %d"},"sort":%s}`, i, i, i, sort))
		}
		fmt.Fprintf(w, `{"hits":{"hits":[%s]}}`, strings.Join(rows, ","))
	}))
	defer node.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{node.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	old := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { opensearchInit.OpenSearchClient = old })
	withChecks(t, map[string]liveness.Checker{})

	p, err := GetUnifiedGlobalSearch(context.Background(), "visitor", "visitor@demo.example", nil, nil, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasMore || p.Next == "" {
		t.Fatalf("a page of 21 hits, one of them a person the demo leaves out: has_more %v, next %q; want more, and a cursor", p.HasMore, p.Next)
	}
	// The next page starts after the 20th hit the index gave: score 81.5,
	// time 1760000000000-19000.
	if after, err := DecodeSearchCursor(p.Next); err != nil || after[0] != 81.5 || after[1] != 1759999981000 {
		t.Errorf("the cursor holds %v (%v); want the 20th hit's score and time", after, err)
	}
	if len(p.Page) != 19 {
		t.Errorf("the page holds %d hits; want the first 20 less the person left out", len(p.Page))
	}
	for _, h := range p.Page {
		if h.User != nil {
			t.Errorf("the demo visitor's page shows %s", h.User.UserEmail)
		}
	}
}
