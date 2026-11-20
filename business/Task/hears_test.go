package business

import (
	"context"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// The one hearer check holds a guest to members' news (an SLA's) and lets
// them hear of the task they read (Urgent); the server's own news leaves
// nobody out as its maker.
func TestAGuestHearsOnlyWhatGuestsMayRead(t *testing.T) {
	hearing, eligible := hearSeams, newTaskEligible
	t.Cleanup(func() { hearSeams, newTaskEligible = hearing, eligible })
	gus, ben := uuid.NewString(), uuid.NewString()
	newTaskEligible = func(_ context.Context, _ string, exclude string, mentioned []string, onlyMentioned bool) ([]string, error) {
		if exclude != uuid.Nil.String() || !onlyMentioned {
			t.Errorf("asked leaving out %q (only mentioned %v): the server's news leaves out no one", exclude, onlyMentioned)
		}
		return mentioned, nil
	}
	hearSeams.readers = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, id := range ids {
			out[id] = true
		}
		return out, nil
	}
	asked := 0
	hearSeams.guest = func(_ context.Context, who string) bool { asked++; return who == gus }
	given := func(who string) *dgraphStruct.DgraphTask {
		return &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Assignee: &dgraphStruct.DgraphUser{Uuid: who},
			Project: &dgraphStruct.DgraphProject{Uuid: uuid.NewString()}}
	}
	if who, ok := AssigneeHears(context.Background(), given(gus), "", MembersOnly); ok {
		t.Errorf("a guest hears members' news: %s", who)
	}
	if who, ok := AssigneeHears(context.Background(), given(ben), "", MembersOnly); !ok || who != ben {
		t.Errorf("a member doesn't hear members' news: %q %v", who, ok)
	}
	asked = 0
	if who, ok := AssigneeHears(context.Background(), given(gus), "", GuestsHear); !ok || who != gus || asked != 0 {
		t.Errorf("a guest doesn't hear of the task they read: %q %v (guest read %d times)", who, ok, asked)
	}
}
