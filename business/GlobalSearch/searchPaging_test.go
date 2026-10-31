package business

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// fakeNode answers every search with the rows row(i) gives for the page asked
// for and the counts, and keeps each request's body.
func fakeNode(t *testing.T, total int, row func(i int) string) *[]map[string]any {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		from, _ := body["from"].(float64)
		size, _ := body["size"].(float64)
		var rows []string
		for i := int(from); i < int(from+size) && i < total; i++ {
			rows = append(rows, row(i))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"hits":{"total":{"value":%d},"hits":[%s]},"aggregations":{"kinds":{"buckets":[{"key":"channels","doc_count":%d}]}}}`,
			total, strings.Join(rows, ","), total)
	}))
	t.Cleanup(node.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{node.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	old := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { opensearchInit.OpenSearchClient = old })
	withChecks(t, map[string]liveness.Checker{})
	return &bodies
}

func channelRow(i int) string {
	return fmt.Sprintf(`{"_index":"channels","_id":"c%d","_source":{"ch_id":"c%d","ch_name":"alpha %d"}}`, i, i, i)
}

// The search page said "20 results" for the first 20 of any number, and
// had no way to the rest (SA-BUG-9). A page with more after it carries a
// cursor, the next page starts where it ended, and every count is the true
// one.
func TestSearchPagesCarryACursorToTheNextPage(t *testing.T) {
	bodies := fakeNode(t, 21, channelRow)
	first, err := GetUnifiedGlobalSearch(context.Background(), accessBusiness.Subject{UserUUID: "me"}, "me@example.test", nil, nil, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Page) != 20 || !first.HasMore || first.Next == "" || first.Total != 21 || first.Totals[KindChannel] != 21 {
		t.Fatalf("a first page of 21 hits: %d hits, has_more %v, next %q, total %d %v; want 20, more, a cursor, 21",
			len(first.Page), first.HasMore, first.Next, first.Total, first.Totals)
	}
	if from := (*bodies)[0]["from"]; from != 0.0 {
		t.Errorf("the first page started at %v", from)
	}
	next, err := GetUnifiedGlobalSearchPage(context.Background(), accessBusiness.Subject{UserUUID: "me"}, "me@example.test", nil, nil, nil, "alpha", first.Next, Within{})
	if err != nil {
		t.Fatal(err)
	}
	if from := (*bodies)[1]["from"]; from != 20.0 {
		t.Errorf("the second page started at %v, want 20", from)
	}
	if len(next.Page) != 1 || next.HasMore || next.Next != "" || next.Total != 21 {
		t.Errorf("the last page: %d hits, has_more %v, next %q, total %d; want 1, no more, no cursor, 21", len(next.Page), next.HasMore, next.Next, next.Total)
	}
}

// A cursor is a place this server wrote, inside the index's window; anything
// else is refused before it reaches a query.
func TestASearchCursorIsAPlaceOrNothing(t *testing.T) {
	if o, err := decodeCursor(encodeCursor(40, "b"), "b"); err != nil || o != 40 {
		t.Fatalf("a cursor this server wrote reads back as %d, %v", o, err)
	}
	if o, err := decodeCursor("", "b"); err != nil || o != 0 {
		t.Fatalf("no cursor is the start: %d, %v", o, err)
	}
	raw := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{
		"not base64!", raw(`{}`), raw(`{"o":-1}`), raw(`{"o":10000}`), raw(`{"o":"20"}`), raw(`[81.5, 1759999981000]`),
		raw(`{"o":1, "script_fields": {}}` + strings.Repeat(" ", 80)),
	} {
		if _, err := decodeCursor(bad, ""); err == nil {
			t.Errorf("the cursor %q was taken", bad)
		}
	}
	if _, err := GetUnifiedGlobalSearchPage(context.Background(), accessBusiness.Subject{UserUUID: "me"}, "me@example.test", nil, nil, nil, "alpha", "nonsense", Within{}); err != ErrBadSearchCursor {
		t.Errorf("a bad cursor: %v", err)
	}
}

// The demo visitor's page says there is more when a person the demo keeps
// from its visitor was among its hits, and its counts leave that person out,
// so the page never says more than it shows.
func TestTheDemoVisitorsPageKeepsShowMore(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	fakeNode(t, 21, func(i int) string {
		if i == 4 {
			// The workspace's owner, whom the demo keeps from its visitor.
			return fmt.Sprintf(`{"_index":"users","_id":"u%d","_source":{"user_id":"u%d","user_name":"owner","user_email":"owner@example.com"}}`, i, i)
		}
		return channelRow(i)
	})
	p, err := GetUnifiedGlobalSearch(context.Background(), accessBusiness.Subject{UserUUID: "visitor"}, "visitor@demo.example", nil, nil, nil, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasMore || p.Next == "" {
		t.Fatalf("a page of 20 hits of 21, one of them a person the demo leaves out: has_more %v, next %q; want more, and a cursor", p.HasMore, p.Next)
	}
	if len(p.Page) != 19 || p.Total != 20 {
		t.Errorf("the page holds %d hits of %d; want 19 of 20 (the person left out, and not counted)", len(p.Page), p.Total)
	}
	for _, h := range p.Page {
		if h.User != nil {
			t.Errorf("the demo visitor's page shows %s", h.User.UserEmail)
		}
	}
}
