//go:build integration

package business

// An imported attachment in a DM or group names its people, by uuid, for the
// search permission filter and for display. They were looked up with a
// function that takes graph uids, which refused uuids, so every attachment
// fell back to bare ids.
// Run: go test -tags=integration ./business/SlackImport/ -run TestChatParticipantsAreFoundByUUID -v

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestChatParticipantsAreFoundByUUID(t *testing.T) {
	dg := integration.SetupDgraph(t)
	ada, bo := uuid.NewString(), uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"uid": "_:ada", "dgraph.type": "User", "user_uuid": ada, "user_name": "ada"},
		{"uid": "_:bo", "dgraph.type": "User", "user_uuid": bo, "user_name": "bo"},
	})

	var got []string
	for _, p := range buildChatParticipants(context.Background(), []string{ada, bo}) {
		got = append(got, p.Uuid+"="+p.Name)
	}
	sort.Strings(got)
	want := []string{ada + "=ada", bo + "=bo"}
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("participants %v, want %v", got, want)
	}
}
