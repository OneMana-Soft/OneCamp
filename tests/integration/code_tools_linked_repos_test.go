//go:build integration
// +build integration

package integration_test

// The AI's code tools read with the GitHub token of the admin who connected
// GitHub, which reaches every repository that admin can (A13). Against real
// Postgres and Dgraph, with GitHub's API stood in and every request to it
// kept: a repository is read only when a project the reader is in links it,
// and in a run someone else asked for, only when they are in one too. Any
// other read is refused before a request is made.
//
// Run: go test -tags=integration ./tests/integration/ -run TestCodeToolsReadOnlyLinkedRepositories -v

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCodeToolsReadOnlyLinkedRepositories(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	github := recordGitHub(t)
	// Website (ana, ben) links acme/web; Billing, which neither is in, links
	// acme/billing; cy is in neither.
	w := seedGitHubWorkspace(t, env, dg)
	joinProject(t, dg, w.project, w.people["ana"], w.people["ben"])

	aiBusiness.RegisterToolExecutors()
	readFile := ai.Executors["read_repo_file"]
	read := func(ctx context.Context, who, owner, repo string) error {
		_, _, err := readFile(ctx, ai.ProposedAction{Params: map[string]string{"owner": owner, "repo": repo, "path": "README.md"}}, w.people[who].String())
		return err
	}

	for _, c := range []struct {
		name string
		ctx  context.Context
		who  string
		repo string
	}{
		{"a repository no project links", ctx, "ana", "acme/payroll"},
		{"a repository only a project they aren't in links", ctx, "ana", "acme/billing"},
		{"their project's repository, for someone outside the project", ctx, "cy", "acme/web"},
		{"ana's agent, in a run cy asked for", ai.WithRunRequester(ctx, w.people["cy"].String(), w.people["ana"].String()), "ana", "acme/web"},
	} {
		owner, name, _ := strings.Cut(c.repo, "/")
		if err := read(c.ctx, c.who, owner, name); !errors.Is(err, aiBusiness.ErrRepoNotLinked) {
			t.Errorf("%s: %v, want %q", c.name, err, aiBusiness.ErrRepoNotLinked)
		}
	}
	if seen := github.calls(); len(seen) != 0 {
		t.Fatalf("refused reads reached GitHub: %v", seen)
	}

	// Ben, in Website, reads its repository, named in any case as GitHub takes it.
	if err := read(ctx, "ben", "ACME", "Web"); err != nil {
		t.Fatalf("ben reading his project's repository: %v", err)
	}
	if seen := github.calls(); len(seen) != 1 || seen[0] != "GET /repos/ACME/Web/contents/README.md" {
		t.Errorf("ben's read asked GitHub %v, want the one file", seen)
	}
}

// joinProject writes the people's side of their membership of project, which
// seedGitHubWorkspace leaves at the project's side; the app writes both.
func joinProject(t *testing.T, dg *integration.DgraphEnv, project string, people ...uuid.UUID) {
	t.Helper()
	ids := make([]string, len(people))
	for i, p := range people {
		ids[i] = `"` + p.String() + `"`
	}
	var got struct {
		P []struct {
			UID string `json:"uid"`
		} `json:"p"`
		U []struct {
			UID string `json:"uid"`
		} `json:"u"`
	}
	raw := dg.Query(t, `{ p(func: eq(project_uuid, "`+project+`")) { uid } u(func: eq(user_uuid, [`+strings.Join(ids, ",")+`])) { uid } }`)
	if err := json.Unmarshal(raw, &got); err != nil || len(got.P) != 1 || len(got.U) != len(people) {
		t.Fatalf("the project and its people in the graph: %v %s", err, raw)
	}
	edges := make([]map[string]any, 0, len(got.U))
	for _, u := range got.U {
		edges = append(edges, map[string]any{"uid": u.UID, "user_projects": []map[string]any{{"uid": got.P[0].UID}}})
	}
	dg.Mutate(t, edges)
}
