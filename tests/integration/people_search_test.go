//go:build integration
// +build integration

package integration_test

// People are found by display name, full name and @handle wherever people
// are searched, against Postgres 12 and Dgraph: the member search behind
// forwarding and the people picker (business/User.GetChannelsAndUsers), the
// doc and board people search (domain/User.GetUserListWithSearchText), and
// the list the @mention picker filters (GetDgraphAllUsersList), which now
// carries each member's full name and handle. When the handles can't be
// read, the names still find people.
//
// Run: go test -tags=integration ./tests/integration/ -run TestPeopleSearch -v

import (
	"context"
	"testing"

	"github.com/google/uuid"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestPeopleSearchByEveryName(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)

	sam, priya, octo, searcher := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, p := range []struct {
		id              uuid.UUID
		email, username string
		external        bool
	}{
		{sam, "sam@example.test", "sam.r", false},
		{priya, "priya@example.test", "priya.raman", false},
		{octo, "octo@example.test", "octo-sam", true},
		{searcher, "me@example.test", "me", false},
	} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, is_external, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, p.id, p.email, p.username, p.external); err != nil {
			t.Fatal(err)
		}
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam.String(), "user_name": "Sam", "user_full_name": "Samuel Rivera", "user_email_id": "sam@example.test"},
		{"uid": "_:priya", "dgraph.type": "User", "user_uuid": priya.String(), "user_name": "PR", "user_full_name": "Priya Raman", "user_email_id": "priya@example.test"},
		{"uid": "_:octo", "dgraph.type": "User", "user_uuid": octo.String(), "user_name": "Octo Cat", "is_external": true},
		{"uid": "_:me", "dgraph.type": "User", "user_uuid": searcher.String(), "user_name": "Me"},
	})

	found := func(term string) map[string]string {
		users, err := userDomain.GetUserListWithSearchText(ctx, searcher.String(), term)
		if err != nil {
			t.Fatalf("search %q: %v", term, err)
		}
		out := map[string]string{}
		for _, u := range users {
			out[u.Uuid] = u.Handle
		}
		return out
	}
	for term, want := range map[string]uuid.UUID{
		"Samuel":      sam,   // full name
		"rivera":      sam,   // full name, any case
		"sam.r":       sam,   // handle
		"@sam.r":      sam,   // handle as typed in a mention
		"priya.raman": priya, // handle with a full stop
		"PR":          priya, // display name
	} {
		got := found(term)
		h, ok := got[want.String()]
		if !ok {
			t.Errorf("%q did not find %s: %v", term, want, got)
			continue
		}
		if h == "" {
			t.Errorf("%q found %s without their handle", term, want)
		}
	}
	if got := found("octo"); len(got) != 0 {
		t.Errorf("an external row was found: %v", got)
	}

	// The forwarding and people-picker search, as its controller calls it.
	list, err := userBusiness.GetChannelsAndUsers(ctx, uids["me"], searcher.String(), "priya.raman")
	if err != nil {
		t.Fatal(err)
	}
	var hit bool
	for _, r := range list {
		if r.Type == "user" && r.UserUuid == priya.String() {
			hit = r.UserHandle == "priya.raman" && r.UserFullName == "Priya Raman"
		}
	}
	if !hit {
		t.Errorf("forward search for priya.raman: %+v", list)
	}

	// The @mention picker's list: each member with their full name and handle.
	all, err := userBusiness.GetDgraphAllUsersList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range all {
		if u.Uuid == sam.String() && (u.Handle != "sam.r" || u.UserFullName != "Samuel Rivera") {
			t.Errorf("mention list has Sam as handle %q, full name %q", u.Handle, u.UserFullName)
		}
	}

	// Last: handles that can't be read (here, their column away) leave the
	// names to find people, rather than failing the search.
	if _, err := env.PG.Exec(`ALTER TABLE users RENAME COLUMN username TO username_away`); err != nil {
		t.Fatal(err)
	}
	samuel := found("Samuel")
	if _, ok := samuel[sam.String()]; !ok || len(samuel) != 1 {
		t.Errorf("with the handles unread, Samuel found %v, want Sam alone", samuel)
	}
	if _, err := env.PG.Exec(`ALTER TABLE users RENAME COLUMN username_away TO username`); err != nil {
		t.Fatal(err)
	}
}
