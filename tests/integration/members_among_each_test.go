//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/tests/integration"
)

// Who of some people is in each of some projects, read for many projects at
// once (510's scan and a re-check of an import's people ask about every
// project an import made): each project answers for its own people only, a
// member or an admin, never someone who has left, with ids answered as they
// were asked; an id that isn't a project's answers nobody; and a read that
// fails is an error, never "nobody is in".
func TestEachProjectAnswersForItsOwnPeople(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	ann, ben, cal, dan := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:ann", "dgraph.type": "User", "user_uuid": ann, "user_name": "Ann"},
		{"uid": "_:ben", "dgraph.type": "User", "user_uuid": ben, "user_name": "Ben"},
		{"uid": "_:cal", "dgraph.type": "User", "user_uuid": cal, "user_name": "Cal", "user_deleted_at": time.Now().UTC().Format(time.RFC3339)},
		{"uid": "_:dan", "dgraph.type": "User", "user_uuid": dan, "user_name": "Dan"},
	})
	people := func(names ...string) []map[string]any {
		out := make([]map[string]any, len(names))
		for i, n := range names {
			out[i] = map[string]any{"uid": uids[n]}
		}
		return out
	}
	launch, notes, board := uuid.NewString(), uuid.NewString(), uuid.NewString()
	dg.Mutate(t, []map[string]any{
		{"dgraph.type": "Project", "project_uuid": launch, "project_name": "Launch", "project_members": people("ann", "cal"), "project_admins": people("ben")},
		{"dgraph.type": "Project", "project_uuid": notes, "project_name": "Notes", "project_members": people("dan")},
		{"dgraph.type": "Project", "project_uuid": board, "project_name": "Board", "project_members": people("ann")},
	})
	annInCapitals := strings.ToUpper(ann)
	got, err := projectDomain.MembersAmongEach(ctx, map[string][]string{
		launch:          {annInCapitals, ben, cal, dan},
		notes:           {ann, dan},
		board:           {ann},
		"not-a-project": {ann},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what, project, who string
		want               bool
	}{
		{"Ann, a member of Launch, asked in capitals", launch, annInCapitals, true},
		{"Ben, an admin of Launch", launch, ben, true},
		{"Cal, deactivated but still on Launch's members", launch, cal, false},
		{"Dan, a member of Notes alone, asked about Launch", launch, dan, false},
		{"Ann, in Launch and Board, asked about Notes", notes, ann, false},
		{"Dan, a member of Notes", notes, dan, true},
		{"Ann, a member of Board", board, ann, true},
		{"Ann, asked about an id that isn't a project's", "not-a-project", ann, false},
	} {
		if got[c.project][c.who] != c.want {
			t.Errorf("%s: in is %v, want %v", c.what, got[c.project][c.who], c.want)
		}
	}

	dg.Break(t)
	if got, err := projectDomain.MembersAmongEach(ctx, map[string][]string{launch: {ann}}); err == nil {
		t.Errorf("with the graph unreachable: %v and no error; want the read's error", got)
	}
}
