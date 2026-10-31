//go:build integration
// +build integration

package integration_test

// Searching a channel's name finds it, against a real OpenSearch mapped as the
// server maps it.
//
// The channels index stores a channel's id as ch_id, and that is all anything
// writes. Global search filtered channel hits on ch_uuid, which no entry has,
// so the Channels group never came back for anyone: "jump to #design by
// search" found nothing, and the search page's Channels chip never showed.
// Only the channels the person is in come back.
//
// Run: go test -tags=integration ./tests/integration/ -run TestSearchFindsTheChannelsYouAreIn -v

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestSearchFindsTheChannelsYouAreIn(t *testing.T) {
	ctx := context.Background()
	// A search's task hits are checked against who may see them in Postgres (models/taskread).
	integration.ConnectServerPostgres(t)
	search := integration.SetupOpenSearch(t)
	// Every index, mapped as the server maps them at boot.
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	me := uuid.NewString()
	design, designOps, payroll := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().Unix()
	// As CreateChannel writes them.
	for id, name := range map[string]string{design: "design", designOps: "design-ops", payroll: "design-payroll"} {
		channelDomain.CreateChannelInOpenSearch(&openSearchStruct.OpenSearchChannel{Uuid: id, ChannelName: name, ChannelCreatedAt: now})
	}

	// found: the channels a search for text, as someone in channels, returns.
	found := func(text string, channels ...string) string {
		t.Helper()
		var mine []*dgraphStruct.DgraphChannel
		for _, c := range channels {
			mine = append(mine, &dgraphStruct.DgraphChannel{Uuid: c})
		}
		p, err := globalSearchBusiness.GetUnifiedGlobalSearch(ctx, accessBusiness.Subject{UserUUID: me}, me+"@example.test", mine, nil, nil, text)
		if err != nil {
			t.Fatalf("searching %q: %v", text, err)
		}
		var ids []string
		for _, h := range p.Page {
			if h.Channel != nil {
				ids = append(ids, h.Channel.Uuid)
			}
		}
		sort.Strings(ids)
		return strings.Join(ids, ",")
	}
	sorted := func(ids ...string) string {
		sort.Strings(ids)
		return strings.Join(ids, ",")
	}

	// Entries show in a search within a second of being written.
	want := sorted(design, designOps)
	deadline := time.Now().Add(20 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		if got = found("design", design, designOps); got == want {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if got != want {
		t.Fatalf("a search for \"design\" by someone in #design and #design-ops found channels [%s], want [%s]: "+
			"the person's channels never came back", got, want)
	}
	if got := found("design", design); got != design {
		t.Errorf("someone in #design alone found [%s]; a channel they aren't in came back", got)
	}
	if got := found("payroll", design, designOps); got != "" {
		t.Errorf("a search for a channel nobody searching is in found [%s]", got)
	}
}
