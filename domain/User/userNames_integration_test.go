//go:build integration

package domain_test

import (
	"context"
	"testing"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A call's speakers are named in one read (they were read one per speaker,
// DG4 and backend audit #22): a uid that is read twice is named once, a node
// that isn't a person and an identity that isn't a graph uid at all (the
// transcription agent's, or anything a client could send) are left out
// rather than reaching the query.
func TestSpeakersAreNamedInOneRead(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ana", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "ana"},
		{"uid": "_:ben", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "ben"},
		{"uid": "_:room", "dgraph.type": "Channel", "ch_uuid": uuid.NewString(), "ch_name": "not a person"},
	})
	ana, ben, room := uids["ana"], uids["ben"], uids["room"]

	got, err := userDomain.UserNamesByDgraphUIDs(ctx, []string{ana, ben, ana, room, "", "0x1) { uid } q(func: has(user_uuid)", "agent-transcriber"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{ana: "ana", ben: "ben"}
	if len(got) != len(want) {
		t.Fatalf("named %d identities %v; want %v", len(got), got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s -> %q, want %q", k, got[k], v)
		}
	}
}
