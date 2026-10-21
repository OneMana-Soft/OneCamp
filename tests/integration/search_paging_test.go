//go:build integration
// +build integration

package integration_test

// Search shows that there is more than a page, and the next page is the
// rest, against a real OpenSearch mapped as the server maps it.
//
// The unified search asked for one page of 21 across every index and the
// search page called the 20 it showed "20 results", with no way to the rest
// (SA-BUG-9). A page with more after it now carries a cursor (search_after on
// the query's own sort), and the page after it holds the rest, once each.
//
// Run: go test -tags=integration ./tests/integration/ -run TestSearchPagesThroughEveryHit -v

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestSearchPagesThroughEveryHit(t *testing.T) {
	ctx := context.Background()
	// Task hits are checked against who may see them in Postgres (models/taskread).
	integration.ConnectServerPostgres(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	// 25 channels the person is in, all called "rollout …".
	me := uuid.NewString()
	var mine []*dgraphStruct.DgraphChannel
	all := map[string]bool{}
	now := time.Now().Unix()
	for i := 0; i < 25; i++ {
		id := uuid.NewString()
		all[id] = true
		mine = append(mine, &dgraphStruct.DgraphChannel{Uuid: id})
		channelDomain.CreateChannelInOpenSearch(&openSearchStruct.OpenSearchChannel{Uuid: id, ChannelName: fmt.Sprintf("rollout %02d", i), ChannelCreatedAt: now - int64(i)})
	}
	// Every entry searchable before the first page: the second page is asked
	// at once, and an entry the index hadn't refreshed yet was missing from it.
	if _, err := opensearchInit.OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{"channels"}}); err != nil {
		t.Fatalf("refreshing the channels index: %v", err)
	}
	channels := func(p globalSearchBusiness.GlobalSearchPagination) []string {
		var ids []string
		for _, h := range p.Page {
			if h.Channel != nil {
				ids = append(ids, h.Channel.Uuid)
			}
		}
		return ids
	}

	// Entries show in a search within a second of being written.
	var first globalSearchBusiness.GlobalSearchPagination
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		p, err := globalSearchBusiness.GetUnifiedGlobalSearch(ctx, me, me+"@example.test", mine, nil, nil, "rollout")
		if err != nil {
			t.Fatal(err)
		}
		if first = p; len(channels(p)) == 20 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if len(channels(first)) != 20 || !first.HasMore || first.Next == "" {
		t.Fatalf("the first page holds %d channels, has_more %v, next %q; want 20, more, and a cursor", len(channels(first)), first.HasMore, first.Next)
	}
	after, err := globalSearchBusiness.DecodeSearchCursor(first.Next)
	if err != nil {
		t.Fatalf("the page's cursor: %v", err)
	}
	second, err := globalSearchBusiness.GetUnifiedGlobalSearchPage(ctx, me, me+"@example.test", mine, nil, nil, "rollout", after, globalSearchBusiness.Within{})
	if err != nil {
		t.Fatal(err)
	}
	if second.HasMore || second.Next != "" {
		t.Errorf("the second page says there is more (has_more %v, next %q); it holds the last of them", second.HasMore, second.Next)
	}
	seen := map[string]int{}
	for _, id := range append(channels(first), channels(second)...) {
		seen[id]++
	}
	if len(seen) != 25 {
		t.Errorf("the two pages hold %d of the 25 channels", len(seen))
	}
	for id, n := range seen {
		if n != 1 || !all[id] {
			t.Errorf("channel %s shows %d times across the pages", id, n)
		}
	}
}
