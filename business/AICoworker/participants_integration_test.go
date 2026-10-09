//go:build integration

package aicoworker

// The coworker's reply in a group chat names the group's people (for their
// last-seen, notifications and search indexing) from the event's participant
// uuids. They were looked up with a function that takes graph uids, which
// refused uuids, so the reply named nobody.
// Run: go test -tags=integration ./business/AICoworker/ -run TestReplyParticipantsAreFoundByUUID -v

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestReplyParticipantsAreFoundByUUID(t *testing.T) {
	dg := integration.SetupDgraph(t)
	ada, bo := uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ada", "dgraph.type": "User", "user_uuid": ada, "user_name": "ada"},
		{"uid": "_:bo", "dgraph.type": "User", "user_uuid": bo, "user_name": "bo"},
	})

	// The event carries the participants as a JSON array.
	var got []string
	for _, u := range resolveParticipantUsers(context.Background(), []interface{}{ada, bo}) {
		got = append(got, u.Uuid+"="+u.Uid)
	}
	sort.Strings(got)
	want := []string{ada + "=" + uids["ada"], bo + "=" + uids["bo"]}
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("participants %v, want %v", got, want)
	}
}
