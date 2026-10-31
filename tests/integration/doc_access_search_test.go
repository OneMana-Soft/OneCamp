//go:build integration
// +build integration

package integration_test

// Global search finds a doc through every source of access the Access
// Resolver knows (A5), against a real OpenSearch mapped as the server maps it:
// a private doc shared with a team is found by the team's members, one in a
// project's space by the project's members, and neither by anyone else. The
// entries carry the doc's teams and space, written whole; every hit is still
// checked against the graph before it is shown.
//
// Run: go test -tags=integration ./tests/integration/ -run TestGlobalSearchFindsDocsThroughTeamsAndSpaces -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestGlobalSearchFindsDocsThroughTeamsAndSpaces(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	people := map[string]uuid.UUID{"owner": uuid.New(), "teammate": uuid.New(), "member": uuid.New(), "stranger": uuid.New()}
	var nodes []map[string]any
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})
	}
	team, project := uuid.NewString(), uuid.NewString()
	nodes = append(nodes,
		map[string]any{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team, "team_name": "Finance",
			"team_deleted_at": "0001-01-01T00:00:00Z", "team_members": []map[string]any{{"uid": "_:teammate"}}},
		map[string]any{"uid": "_:project", "dgraph.type": "Project", "project_uuid": project, "project_name": "Launch",
			"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": []map[string]any{{"uid": "_:member"}}},
	)
	uids := dg.Mutate(t, nodes)
	person := func(name string) *dgraphStruct.DgraphUser {
		return &dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name}
	}
	priv := true
	budget, err := docBusiness.CreateDocInTree(ctx, person("owner"), docBusiness.NewDoc{Title: "Quarterly budget", Private: &priv}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := docBusiness.CreateDocInTree(ctx, person("owner"), docBusiness.NewDoc{Title: "Launch checklist", Private: &priv, Space: "project:" + project}, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := accessBusiness.Subject{UserUID: uids["owner"], UserUUID: people["owner"].String()}
	if _, err := accessBusiness.Apply(ctx, owner, accessBusiness.Change{
		Resource: accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: budget.Uuid},
		Add:      []accessBusiness.Grant{{Kind: accessBusiness.PrincipalTeam, UUID: team, Level: accessBusiness.LevelView}},
	}); err != nil {
		t.Fatal(err)
	}
	// A change to a doc's sharing writes its entry again; so does a new doc.
	docBusiness.RepairAccessIndexes(ctx, plan.Uuid)

	finds := func(who, text, id string, teams []*dgraphStruct.DgraphTeam, projects []*dgraphStruct.DgraphProject) bool {
		t.Helper()
		p, err := globalSearchBusiness.GetUnifiedGlobalSearch(ctx, accessBusiness.Subject{UserUUID: people[who].String()}, people[who].String()+"@example.test", nil, projects, teams, text)
		if err != nil {
			t.Fatalf("searching %q as %s: %v", text, who, err)
		}
		countsOnlyWhatShows(t, who+" searching "+text, p)
		for _, h := range p.Page {
			if h.Doc != nil && h.Doc.Uuid == id {
				return true
			}
		}
		return false
	}
	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("timed out waiting until %s", what)
	}
	theTeam := []*dgraphStruct.DgraphTeam{{Uuid: team}}
	theProject := []*dgraphStruct.DgraphProject{{Uuid: project}}

	eventually("the team's member finds the budget", func() bool { return finds("teammate", "Quarterly budget", budget.Uuid, theTeam, nil) })
	eventually("the project's member finds the checklist", func() bool { return finds("member", "Launch checklist", plan.Uuid, nil, theProject) })
	if finds("stranger", "Quarterly budget", budget.Uuid, nil, nil) || finds("stranger", "Launch checklist", plan.Uuid, nil, nil) {
		t.Error("someone in neither the team nor the project finds the docs")
	}
	// Claiming a team or a project isn't being in it: the graph decides.
	if finds("stranger", "Quarterly budget", budget.Uuid, theTeam, nil) || finds("stranger", "Launch checklist", plan.Uuid, nil, theProject) {
		t.Error("a search that names a team or project the person isn't in finds its docs")
	}
	if !finds("owner", "Quarterly budget", budget.Uuid, nil, nil) {
		t.Error("the owner doesn't find their own doc")
	}

	// Taking the team off: its member stops finding it.
	if _, err := accessBusiness.Apply(ctx, owner, accessBusiness.Change{
		Resource: accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: budget.Uuid},
		Remove:   []accessBusiness.Grant{{Kind: accessBusiness.PrincipalTeam, UUID: team}},
	}); err != nil {
		t.Fatal(err)
	}
	eventually("the team's member stops finding the budget", func() bool { return !finds("teammate", "Quarterly budget", budget.Uuid, theTeam, nil) })
}
