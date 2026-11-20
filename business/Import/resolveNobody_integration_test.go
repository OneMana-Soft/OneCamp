//go:build integration

package business

// V-1191: a source's person the import mapped to someone who isn't here (no
// row) is no one here: their work keeps its source name and is nobody's,
// never a zero person's (uuid.Nil). Someone who is here is still resolved.
//
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> 'TestAPersonMappedToNobodyResolvesToNoOne'

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAPersonMappedToNobodyResolvesToNoOne(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	ada, ghost := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'ada@acme.test', NOW(), NOW())`, ada); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": ada.String(), "user_name": "ada"})
	job := &importModels.Job{Id: uuid.New(), Provider: "asana", SourceWorkspaceName: "Acme", Source: "api", Status: importModels.StatusRunning}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	for src, id := range map[string]uuid.UUID{"user-ada": ada, "user-gone": ghost} {
		if err := importModels.RecordMapping(ctx, job.Id, job.Provider, job.SourceWorkspaceName, importModels.EntityUser, src, id, nil, nil, true); err != nil {
			t.Fatal(err)
		}
	}

	match, info := resolveUser(ctx, job, "user-gone")
	if match.ID != ghost || info != nil {
		t.Errorf("a person mapped to nobody: match %v, info %+v; want the match and no one", match.ID, info)
	}
	match, info = resolveUser(ctx, job, "user-ada")
	if match.ID != ada || info == nil || info.UserPostgresInfo.Id != ada || info.UserDgraphInfo.Uuid != ada.String() {
		t.Errorf("a person mapped to ada: match %v, info %+v; want ada", match.ID, info)
	}
}
