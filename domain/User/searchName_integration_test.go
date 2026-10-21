//go:build integration

package domain_test

import (
	"context"
	"slices"
	"testing"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// searchedNames is the user names a people search finds for term.
func searchedNames(t *testing.T, term string) []string {
	t.Helper()
	people, err := userDomain.SearchPeople(context.Background(), term, 10)
	if err != nil {
		t.Fatalf("SearchPeople(%q): %v", term, err)
	}
	var got []string
	for _, p := range people {
		got = append(got, p.UserName)
	}
	slices.Sort(got)
	return got
}

// Someone saved before user_search_name existed has none, so a search that
// starts from it can't find them by a term long enough for its index; the
// backfill gives them one, and a second run finds nothing left to write.
//
// Run: go test -tags=integration ./domain/User/ -run '^TestAPersonSavedBeforeTheSearchNameIsFoundAfterTheBackfill$' -v
func TestAPersonSavedBeforeTheSearchNameIsFoundAfterTheBackfill(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	dg.Mutate(t, []map[string]any{
		{"uid": "_:old", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "olga", "user_full_name": "Olga Petrova", "user_department": "Finance"},
		{"uid": "_:empty", "dgraph.type": "User", "user_uuid": uuid.NewString()},
	})

	if got := searchedNames(t, "petrov"); got != nil {
		t.Fatalf("before the backfill a search starting from the index found %v; it should find nobody, or this test proves nothing", got)
	}
	written, err := userDomain.BackfillSearchNames(ctx)
	if err != nil || written != 1 {
		t.Fatalf("the backfill wrote %d (err %v), want 1: Olga, not the person with no names", written, err)
	}
	for _, term := range []string{"petrov", "OLGA", "financ", "ol"} {
		if got := searchedNames(t, term); !slices.Equal(got, []string{"olga"}) {
			t.Errorf("after the backfill SearchPeople(%q) = %v, want [olga]", term, got)
		}
	}
	if again, err := userDomain.BackfillSearchNames(ctx); err != nil || again != 0 {
		t.Errorf("a second backfill wrote %d (err %v), want 0", again, err)
	}
}

// Every save writes the search name from the names the person will hold: a
// new person, a save of one field (the others stay as stored), a rename.
//
// Run: go test -tags=integration ./domain/User/ -run '^TestEverySaveKeepsTheSearchNameInStep$' -v
func TestEverySaveKeepsTheSearchNameInStep(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.SetupDgraph(t)

	id := uuid.NewString()
	uid, err := userDomain.CreateOrUpdateDgraphUser(ctx, &dgraphStruct.DgraphUser{Uuid: id, UserName: "rafa", UserFullName: "Rafael Costa"})
	if err != nil || uid == "" {
		t.Fatalf("creating: %q %v", uid, err)
	}
	if got := searchedNames(t, "costa"); !slices.Equal(got, []string{"rafa"}) {
		t.Errorf("a new person by full name: %v", got)
	}
	if _, err := userDomain.CreateOrUpdateDgraphUser(ctx, &dgraphStruct.DgraphUser{Uuid: id, Title: "Data Engineer"}); err != nil {
		t.Fatal(err)
	}
	if got := searchedNames(t, "engineer"); !slices.Equal(got, []string{"rafa"}) {
		t.Errorf("by the job title just saved: %v", got)
	}
	if got := searchedNames(t, "costa"); !slices.Equal(got, []string{"rafa"}) {
		t.Errorf("saving the job title lost the full name from the search name: %v", got)
	}
	if _, err := userDomain.CreateOrUpdateDgraphUser(ctx, &dgraphStruct.DgraphUser{Uuid: id, UserFullName: "Rafael Souza"}); err != nil {
		t.Fatal(err)
	}
	if got := searchedNames(t, "souza"); !slices.Equal(got, []string{"rafa"}) {
		t.Errorf("by the new full name: %v", got)
	}
	if got := searchedNames(t, "costa"); got != nil {
		t.Errorf("by the full name they no longer have: %v", got)
	}
	if written, err := userDomain.BackfillSearchNames(ctx); err != nil || written != 0 {
		t.Errorf("after saves through CreateOrUpdateDgraphUser the backfill wrote %d (err %v), want 0", written, err)
	}
}
