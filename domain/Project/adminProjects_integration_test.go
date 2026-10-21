//go:build integration

package Domain_test

import (
	"context"
	"slices"
	"testing"
	"time"

	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/tests/integration"
)

// The live projects a person admins are read from that person's side
// (~project_admins), not by filtering every project in the workspace; the
// answer is the same: the ones they admin, archived ones left out.
func TestAdminProjectsAreReadFromTheAdmin(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	live, gone := "0001-01-01T00:00:00Z", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:me", "dgraph.type": "User", "user_uuid": "me-uuid", "user_name": "me"},
		{"uid": "_:other", "dgraph.type": "User", "user_uuid": "other-uuid", "user_name": "other"},
		{"uid": "_:a", "dgraph.type": "Project", "project_uuid": "a", "project_name": "Alpha", "project_deleted_at": live, "project_admins": []map[string]string{{"uid": "_:me"}}},
		{"uid": "_:b", "dgraph.type": "Project", "project_uuid": "b", "project_name": "Beta", "project_deleted_at": live, "project_admins": []map[string]string{{"uid": "_:me"}, {"uid": "_:other"}}},
		{"uid": "_:c", "dgraph.type": "Project", "project_uuid": "c", "project_name": "Gone", "project_deleted_at": gone, "project_admins": []map[string]string{{"uid": "_:me"}}},
		{"uid": "_:d", "dgraph.type": "Project", "project_uuid": "d", "project_name": "Theirs", "project_deleted_at": live, "project_admins": []map[string]string{{"uid": "_:other"}}},
	})
	projects, err := projectDomain.GetDgraphProjectListByAdminDgraphUID(ctx, uids["me"])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range projects {
		got = append(got, p.Name)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"Alpha", "Beta"}) {
		t.Errorf("admin projects: %v, want Alpha and Beta", got)
	}
}
