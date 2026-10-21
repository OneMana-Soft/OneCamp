//go:build integration

package domain_test

import (
	"context"
	"slices"
	"testing"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// People are found by name, full name, job title and department, short terms
// included; bots and guests never are. The department clause asked for
// <user.department>, a predicate the schema doesn't have (it is
// user_department), so nobody was ever found by their department. The
// people are saved as they were before user_search_name, where the searches
// start, so the backfill gives them theirs first.
func TestPeopleAreFoundByEveryNameTheyHave(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)

	alice, bob, bot, guest := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username) VALUES ($1, 'bob@example.test', 'bobby_handle')`, bob); err != nil {
		t.Fatal(err)
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:alice", "dgraph.type": "User", "user_uuid": alice.String(), "user_name": "alice", "user_full_name": "Alice Smith", "user_job_title": "Product Designer", "user_department": "Marketing", "user_email_id": "alice@example.test"},
		{"uid": "_:bob", "dgraph.type": "User", "user_uuid": bob.String(), "user_name": "bob", "user_full_name": "Bob Jones", "user_job_title": "Engineer", "user_email_id": "bob@example.test"},
		{"uid": "_:bot", "dgraph.type": "User", "user_uuid": bot.String(), "user_name": "alicebot", "user_full_name": "Alice Bot", "is_bot": true, "user_department": "Marketing"},
		{"uid": "_:guest", "dgraph.type": "User", "user_uuid": guest.String(), "user_name": "aliceguest", "user_full_name": "Alice Guest", "is_external": true, "user_department": "Marketing"},
	})
	if len(uids) != 4 {
		t.Fatalf("seeded %d people", len(uids))
	}
	if n, err := userDomain.BackfillSearchNames(ctx); err != nil || n != 4 {
		t.Fatalf("the search-name backfill wrote %d (err %v), want 4", n, err)
	}

	names := func(_ *testing.T, got []string) []string { slices.Sort(got); return got }
	for _, c := range []struct {
		term string
		want []string
	}{
		{"market", []string{"alice"}}, // department
		{"design", []string{"alice"}}, // job title
		{"smith", []string{"alice"}},  // full name
		{"ali", []string{"alice"}},    // name
		{"al", []string{"alice"}},     // too short for the index
		{"ENGIN", []string{"bob"}},    // any case
		{"nobody", nil},
	} {
		people, err := userDomain.SearchPeople(ctx, c.term, 10)
		if err != nil {
			t.Fatalf("SearchPeople(%q): %v", c.term, err)
		}
		var got []string
		for _, p := range people {
			got = append(got, p.UserName)
		}
		if g := names(t, got); !slices.Equal(g, c.want) {
			t.Errorf("SearchPeople(%q) = %v, want %v", c.term, g, c.want)
		}
	}

	for _, c := range []struct {
		term string
		want []string
	}{
		{"smi", []string{"alice"}},
		{"sm", []string{"alice"}},
		{"alice", []string{"alice", "alicebot"}}, // the member list keeps bots, never guests
		{"bobby_h", []string{"bob"}},             // by handle, from Postgres
		{"@bobby", []string{"bob"}},
	} {
		people, err := userDomain.GetUserListWithSearchText(ctx, alice.String(), c.term)
		if err != nil {
			t.Fatalf("GetUserListWithSearchText(%q): %v", c.term, err)
		}
		var got []string
		for _, p := range people {
			got = append(got, p.UserName)
		}
		if g := names(t, got); !slices.Equal(g, c.want) {
			t.Errorf("GetUserListWithSearchText(%q) = %v, want %v", c.term, g, c.want)
		}
	}

	byUID, err := userDomain.GetDgraphUserInfoByUUIDs(ctx, []string{uids["alice"], uids["bob"]})
	if err != nil {
		t.Fatal(err)
	}
	if len(byUID) != 2 {
		t.Errorf("by graph id: %d people, want 2", len(byUID))
	}
}
