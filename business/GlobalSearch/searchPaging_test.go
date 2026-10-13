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
	next, err := GetUnifiedGlobalSearchPage(context.Background(), "me", "me@example.test", nil, nil, nil, "alpha", after)
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
