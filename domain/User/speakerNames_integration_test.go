//go:build integration

package domain_test

import (
	"context"
	"testing"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A call's speakers are named in one read (backend audit #22): each was a
// read of the whole person. Anything that isn't a person's graph uid is left
// out, never put into the query.
func TestACallsSpeakersAreNamedInOneRead(t *testing.T) {
	dg := integration.SetupDgraph(t)
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": "u-maya", "user_name": "Maya"},
		{"uid": "_:bo", "dgraph.type": "User", "user_uuid": "u-bo", "user_name": "Bo"},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": "t1", "team_name": "Eng"},
	})
	got, err := userDomain.UserNamesByDgraphUIDs(context.Background(), []string{uids["maya"], uids["bo"], uids["team"], "0x1) { x } }", "", uids["maya"]})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[uids["maya"]] != "Maya" || got[uids["bo"]] != "Bo" {
		t.Errorf("speakers: %v", got)
	}
}
