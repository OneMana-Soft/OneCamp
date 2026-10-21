//go:build integration

package integration_test

import (
	"context"
	"net/url"
	"testing"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/integration/dgfixture"
)

// The board's first answer with the web's first page per open column (DG4b),
// then each open column walked by its pages, is the whole board: every card
// the old answer (a thousand a column) sent, in the same order, once.
func TestABoardColumnWalksToTheWholeColumn(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	integration.SetupDgraph(t)
	f, err := dgfixture.Write(ctx, dgfixture.Small, dgfixture.DefaultSeed, dgraphInit.DgraphClient, env.PG)
	if err != nil {
		t.Fatal(err)
	}
	none := url.Values{}
	whole, err := projectBusiness.GetDgraphProjectTaskListForKanban(ctx, f.BoardProject, f.ViewerUID(), "", helpers.OpenLimit(none), helpers.ClosedLimit(none))
	if err != nil {
		t.Fatal(err)
	}
	first, err := projectBusiness.GetDgraphProjectTaskListForKanban(ctx, f.BoardProject, f.ViewerUID(), "", 60, helpers.ClosedLimit(none))
	if err != nil {
		t.Fatal(err)
	}
	walked := 0
	for _, c := range dgraphStruct.BoardColumns {
		if c.Closed {
			continue
		}
		want := *whole.Column(c.Key)
		got := append([]*dgraphStruct.DgraphTask(nil), *first.Column(c.Key)...)
		// Only the column's own-status cards page here; a custom status's
		// cards would page under its own id (taskrank's tests cover that).
		var own []*dgraphStruct.DgraphTask
		for _, card := range got {
			if card.CustomStatus == nil || *card.CustomStatus == "" {
				own = append(own, card)
			}
		}
		got = own
		// The first page is per part of a column (its own status and each
		// custom status in it), as the board's Window picks.
		if len(got) > 60 {
			t.Fatalf("%s: the first answer sent %d own-status cards, past its page of 60", c.Key, len(got))
		}
		var wantOwn []*dgraphStruct.DgraphTask
		for _, card := range want {
			if card.CustomStatus == nil || *card.CustomStatus == "" {
				wantOwn = append(wantOwn, card)
			}
		}
		for more := len(got) > 0 && len(got) < len(wantOwn); more; {
			page, err := projectBusiness.GetProjectBoardColumnPage(ctx, f.BoardProject, f.ViewerUID(), "", c.Status, got[len(got)-1].Uuid, 60)
			if err != nil {
				t.Fatalf("%s after %d: %v", c.Key, len(got), err)
			}
			got = append(got, page.Cards...)
			more = page.More
		}
		if len(got) != len(wantOwn) {
			t.Fatalf("%s: walked %d cards, the whole board had %d", c.Key, len(got), len(wantOwn))
		}
		for i := range got {
			if got[i].Uuid != wantOwn[i].Uuid {
				t.Fatalf("%s: card %d is %s, the whole board had %s there", c.Key, i, got[i].Uuid, wantOwn[i].Uuid)
			}
		}
		walked += len(got)
	}
	if walked < 500 {
		t.Fatalf("walked %d cards; the fixture's big board has hundreds of own-status open cards", walked)
	}
}
