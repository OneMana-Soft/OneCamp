//go:build integration
// +build integration

package integration_test

// What Ask AI and search read per request, counted by the store-budget taps
// (storebudget.go), for the reads H6 batches: the connectors a person has
// connected (business/Connector) and whether results still exist
// (domain/Liveness). Each count is the calls on the request's goroutine and
// on any other it started, so work moved onto a goroutine still counts.

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	integrationDomain "github.com/akashc777/OneCamp/domain/Integration"
	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/tests/integration"
)

func askTaps(t *testing.T) *integration.StoreTaps {
	t.Helper()
	env := integration.SetupEnv(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	return integration.TapStores(t, env, dg, search)
}

func pgCalls(w integration.Window) int {
	return w.Sync[integration.StorePostgres] + w.Async[integration.StorePostgres]
}

// TestConnectedProvidersAreReadInOneStatement: Ask AI names the connectors
// the asker has connected in one statement for every provider, not one each
// (H6 A1-14), and names the same ones: a live row whose access token is set
// and decrypts. An empty token, a token that does not decrypt and a
// disconnected (soft-deleted) row are not connected.
func TestConnectedProvidersAreReadInOneStatement(t *testing.T) {
	ctx := context.Background()
	taps := askTaps(t)
	providers := connectorBusiness.Providers()
	if len(providers) < 4 {
		t.Fatalf("%d providers registered; the case needs 4", len(providers))
	}
	person := uuid.New()
	sealed, err := helpers.EncryptSecret("an access token")
	if err != nil {
		t.Fatal(err)
	}
	empty, unreadable := "", "not sealed"
	connect := func(provider string, token *string) {
		t.Helper()
		if err := integrationDomain.UpsertIntegration(ctx, "user", person, provider, token, nil, nil, nil, nil); err != nil {
			t.Fatalf("connect %s: %v", provider, err)
		}
	}
	connect(providers[0].ID, &sealed)
	connect(providers[1].ID, &empty)
	connect(providers[2].ID, &unreadable)
	connect(providers[3].ID, &sealed)
	if err := integrationDomain.DeleteIntegration(ctx, "user", person, providers[3].ID, person); err != nil {
		t.Fatalf("disconnect %s: %v", providers[3].ID, err)
	}

	var names []string
	w, err := taps.Measure(func() { names = connectorBusiness.ConnectedProviderNames(ctx, person.String()) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{providers[0].Name}; !slices.Equal(names, want) {
		t.Errorf("connected %q; want %q", names, want)
	}
	if n := pgCalls(w); n != 1 {
		t.Errorf("%d providers took %d statements (%q); want 1", len(providers), n, append(w.SyncCalls, w.AsyncCalls...))
	}

	w, err = taps.Measure(func() { names = connectorBusiness.ConnectedProviderNames(ctx, uuid.New().String()) })
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 || pgCalls(w) != 1 {
		t.Errorf("someone with nothing connected: %q in %d statements; want none in 1", names, pgCalls(w))
	}
}

// TestMixedLivenessIdsAreCheckedInOneStatement: whether results of several
// kinds kept in Postgres still exist is one statement for all their tables,
// not one per table (H6 A1-12), and every kind is still checked: an id with
// no row is gone, whichever table it is in.
func TestMixedLivenessIdsAreCheckedInOneStatement(t *testing.T) {
	ctx := context.Background()
	taps := askTaps(t)
	type item struct{ kind, id string }
	items := []item{{"post", uuid.NewString()}, {"chat", uuid.NewString()}, {"comment", uuid.NewString()}, {"post", uuid.NewString()}}
	var states map[string]map[string]bool
	w, err := taps.Measure(func() {
		states = liveness.States(ctx, liveness.Checkers, items, func(i item) (string, string) { return i.kind, i.id })
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		state, checked := states[it.kind]
		if !checked {
			t.Errorf("%s was not checked", it.kind)
			continue
		}
		if _, there := state[it.id]; there {
			t.Errorf("%s %s has no row, but was found", it.kind, it.id)
		}
	}
	if n := pgCalls(w); n != 1 {
		t.Errorf("posts, chats and comments took %d statements (%q); want 1", n, append(w.SyncCalls, w.AsyncCalls...))
	}
}
