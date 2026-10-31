//go:build integration

package business

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A form rule runs when its form is sent, once per submission however often
// the event is delivered, and never on another form's; a form that adds
// table rows takes no rule.
//
// Run: go test -tags=integration ./business/Workflow/ -run TestAFormRuleRunsOncePerSubmission -v
func TestAFormRuleRunsOncePerSubmission(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	user, project, form := uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, user, user.String()[:8]+"@example.test"); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:u", "user_uuid": user.String(), "user_name": "maya"},
		{"uid": "_:p", "project_uuid": project.String(), "project_name": "Q4 launch",
			"project_members": []map[string]any{{"uid": "_:u"}}},
	})
	// The project's form, which the rule is saved against.
	if _, err := env.PG.Exec(`INSERT INTO project_forms (id, project_uuid, token, title, fields, created_by)
		VALUES ($1, $2, 'cdefghijklmnopqrstuvwxyz', 'Requests', '[]', $3)`, form, project, user); err != nil {
		t.Fatal(err)
	}
	who := &userModels.UserInfo{}
	who.UserPostgresInfo.Id = user
	who.UserDgraphInfo.Uuid = user.String()
	w, err := CreateProjectRule(ctx, who, project, WorkflowInput{
		Name:          "Bugs from the form",
		IsActive:      true,
		TriggerType:   workflowModel.TriggerFormSubmitted,
		TriggerConfig: map[string]interface{}{"form_id": form.String()},
		Actions:       []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A form that adds table rows makes no task for a rule to run on.
	rows := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO project_forms (id, project_uuid, token, title, fields, created_by, target_table)
		VALUES ($1, $2, 'defghijklmnopqrstuvwxyza', 'Vendors', '[]', $3, $4)`, rows, project, user, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProjectRule(ctx, who, project, WorkflowInput{Name: "On a row", IsActive: true, TriggerType: workflowModel.TriggerFormSubmitted,
		TriggerConfig: map[string]interface{}{"form_id": rows.String()}, Actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}}); err == nil {
		t.Error("a rule on a form that adds table rows was saved")
	}
	runs := func() int {
		t.Helper()
		rs, err := workflowModel.ListRuns(ctx, nil, &w.Id, nil, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(rs)
	}
	sent := func(formID, submission string) {
		handleEvent(ctx, "form.submitted", map[string]interface{}{
			"event_id": submission, "form_id": formID, "form_title": "Requests", "task_id": uuid.NewString(),
			"project_id": project.String(), "name": "Checkout crashes", "created_by": user.String(),
			"answers": map[string]interface{}{"kind": "Bug"},
		})
	}
	sent(uuid.NewString(), uuid.NewString())
	if n := runs(); n != 0 {
		t.Fatalf("another form's submission ran the rule (%d runs)", n)
	}
	one := uuid.NewString()
	sent(form.String(), one)
	sent(form.String(), one) // delivered twice
	if n := runs(); n != 1 {
		t.Fatalf("one submission ran the rule %d times", n)
	}
	sent(form.String(), uuid.NewString())
	if n := runs(); n != 2 {
		t.Fatalf("a second submission left the rule at %d runs", n)
	}
}
