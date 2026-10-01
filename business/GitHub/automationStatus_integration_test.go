//go:build integration

package business

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A rule that moves tasks to a project's own status keeps working when the
// status is renamed, follows its tasks when it is deleted, and a status the
// project does not have is refused when the rule is saved.
func TestAutomationRulesFollowCustomStatuses(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupDgraph(t)

	user, project := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, user, user.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name) VALUES ($1, $2)`, project, "rules "+project.String()[:8]); err != nil {
		t.Fatal(err)
	}
	link, err := LinkRepositoryToProject(ctx, LinkRepoInput{ProjectId: project, RepoOwner: "acme", RepoName: "web"}, user)
	if err != nil {
		t.Fatal(err)
	}
	qa, err := taskStatusBusiness.Create(ctx, project, user, taskStatusBusiness.Input{Name: "QA", Category: "inReview"})
	if err != nil {
		t.Fatal(err)
	}
	rulesOf := func() map[string]string {
		t.Helper()
		l, err := GetGitHubLinkById(ctx, link.Id)
		if err != nil {
			t.Fatal(err)
		}
		return parseAutomationRules(l)
	}
	save := func(in map[string]string) error {
		rules, err := NormalizeAutomationRules(ctx, project, in)
		if err != nil {
			return err
		}
		b, err := json.Marshal(rules)
		if err != nil {
			return err
		}
		return UpdateAutomationRulesAndInvalidate(ctx, link.Id, string(b))
	}

	t.Run("saving stores ids and keys, drops no-change, refuses unknown statuses", func(t *testing.T) {
		if err := save(map[string]string{"pr_merged": "Done", "review_requested": "qa", "pr_opened": "_none"}); err != nil {
			t.Fatal(err)
		}
		got := rulesOf()
		if got["pr_merged"] != "done" || got["review_requested"] != qa.ID.String() || len(got) != 2 {
			t.Fatalf("%v", got)
		}
		if err := save(map[string]string{"pr_merged": "Shipped"}); !errors.Is(err, taskStatusBusiness.ErrUnknownStatus) {
			t.Fatalf("an unknown status was accepted: %v", err)
		}
	})

	t.Run("a rule that names the status by name follows a rename", func(t *testing.T) {
		// As an older row, or an API client, might have stored it.
		if err := UpdateAutomationRulesAndInvalidate(ctx, link.Id, `{"approved":"QA","pr_merged":"done"}`); err != nil {
			t.Fatal(err)
		}
		if _, err := taskStatusBusiness.Update(ctx, project, qa.ID, taskStatusBusiness.Input{Name: "Testing", Category: "inReview"}); err != nil {
			t.Fatal(err)
		}
		if got := rulesOf(); got["approved"] != qa.ID.String() || got["pr_merged"] != "done" {
			t.Fatalf("%v", got)
		}
	})

	t.Run("deleting the status points its rules where its tasks went", func(t *testing.T) {
		blocked, err := taskStatusBusiness.Create(ctx, project, user, taskStatusBusiness.Input{Name: "Blocked", Category: "inProgress"})
		if err != nil {
			t.Fatal(err)
		}
		if err := UpdateAutomationRulesAndInvalidate(ctx, link.Id, `{"approved":"`+qa.ID.String()+`","changes_requested":"`+blocked.ID.String()+`"}`); err != nil {
			t.Fatal(err)
		}
		if err := taskStatusBusiness.Delete(ctx, project, qa.ID, blocked.ID.String()); err != nil {
			t.Fatal(err)
		}
		if got := rulesOf(); got["approved"] != blocked.ID.String() || got["changes_requested"] != blocked.ID.String() {
			t.Fatalf("after deleting QA into Blocked: %v", got)
		}
		if err := taskStatusBusiness.Delete(ctx, project, blocked.ID, ""); err != nil {
			t.Fatal(err)
		}
		if got := rulesOf(); got["approved"] != "inProgress" || got["changes_requested"] != "inProgress" {
			t.Fatalf("after deleting Blocked back to its category: %v", got)
		}
	})
}
