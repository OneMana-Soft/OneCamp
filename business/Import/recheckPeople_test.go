package business

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"

	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
)

// Only the people still in their project are kept, asking the graph once
// about every project: 510's scan and a re-check of an import's people.
func TestOnlyThoseStillInTheirProjectAreKept(t *testing.T) {
	launch, notes := uuid.New(), uuid.New()
	ann, ben := uuid.New(), uuid.New()
	var asks []map[string][]string
	was := membersAmongEach
	defer func() { membersAmongEach = was }()
	membersAmongEach = func(_ context.Context, asked map[string][]string) (map[string]map[string]bool, error) {
		asks = append(asks, asked)
		return map[string]map[string]bool{launch.String(): {ann.String(): true}, notes.String(): {}}, nil
	}
	got, err := StillMembers(context.Background(), []reviewModel.Candidate{
		{ProjectUUID: launch, UserID: ann}, {ProjectUUID: launch, UserID: ben}, {ProjectUUID: notes, UserID: ann},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ProjectUUID != launch || got[0].UserID != ann {
		t.Errorf("kept %v, want Ann in Launch alone", got)
	}
	want := map[string][]string{launch.String(): {ann.String(), ben.String()}, notes.String(): {ann.String()}}
	if len(asks) != 1 || !maps.EqualFunc(asks[0], want, slices.Equal) {
		t.Errorf("asked the graph %v, want once, about both projects' people", asks)
	}
}

// An import's people, by how its source has them now: a client is one any of
// whose source people the source keeps out of projects, though another joins
// them; one who joins and is no longer listed under another person joins;
// one it lists under no person is unlisted; and when it lists none of them,
// everyone is.
func TestAnImportsPeopleAreSortedByHowTheSourceHasThemNow(t *testing.T) {
	ben, cleo, dan, eve := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	users := map[string]uuid.UUID{"u-ben": ben, "u-cleo": cleo, "u-cleo-old": cleo, "u-dan": dan, "u-dan-old": dan, "u-eve": eve}
	set := func(ids ...uuid.UUID) map[uuid.UUID]bool {
		out := map[uuid.UUID]bool{}
		for _, id := range ids {
			out[id] = true
		}
		return out
	}
	clients, joining, unlisted := sortPeople(users, map[string]bool{"u-ben": false, "u-cleo": true, "u-cleo-old": false, "u-dan": false})
	if !maps.Equal(clients, set(cleo)) || !maps.Equal(joining, set(ben, dan)) || !maps.Equal(unlisted, set(eve)) {
		t.Errorf("clients %v, joining %v, unlisted %v; want Cleo, Ben and Dan, Eve", clients, joining, unlisted)
	}
	clients, joining, unlisted = sortPeople(users, map[string]bool{"u-zed": false})
	if len(clients) != 0 || len(joining) != 0 || !maps.Equal(unlisted, set(ben, cleo, dan, eve)) {
		t.Errorf("listing none of them: clients %v, joining %v, unlisted %v; want everyone unlisted", clients, joining, unlisted)
	}
}
