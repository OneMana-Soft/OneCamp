//go:build integration
// +build integration

package integration_test

// A save of a task field keeps the options it doesn't list, and takes out only
// those it names in removed_options: the rule a table's select column follows
// too (helpers.KeptOptions).
//
// A save took out every stored option it didn't list, and with it every task's
// value of that option. A page opened before an option was added (a second
// tab, a dialog left open) saved without it, and the tasks set to it lost
// their value without a word.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAStaleFieldSaveKeepsTheOptionsItDoesntName -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	taskFieldController "github.com/akashc777/OneCamp/controllers/TaskField"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	fieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAStaleFieldSaveKeepsTheOptionsItDoesntName(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	w := seedGitHubWorkspace(t, env, dg) // Website, its admin ana, three of its tasks
	project := uuid.MustParse(w.project)
	asAna := w.as("ana")
	ana, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people["ana"].String())
	if err != nil || ana == nil {
		t.Fatalf("ana's node: %v", err)
	}

	// "Stage" has Todo and Doing when the first tab opens it; a second tab adds
	// Blocked and sets a task to it.
	field, err := taskFieldBusiness.Create(ctx, project, taskFieldBusiness.Input{Name: "Stage", Type: taskFieldBusiness.TypeSelect,
		Options: []taskFieldBusiness.OptionInput{{Label: "Todo"}, {Label: "Doing"}}}, w.people["ana"], "")
	if err != nil {
		t.Fatal(err)
	}
	read := field.Options
	field, err = taskFieldBusiness.AddOptions(ctx, project, field.ID, []taskFieldBusiness.OptionInput{{Label: "Blocked"}})
	if err != nil || len(field.Options) != 3 {
		t.Fatalf("adding Blocked: %+v %v", field, err)
	}
	blocked := field.Options[2].ID
	if _, err := taskFieldBusiness.SetValue(ctx, &dgraphStruct.DgraphTask{Uuid: w.task}, field, json.RawMessage(`"`+blocked+`"`), ana, w.people["ana"]); err != nil {
		t.Fatal(err)
	}

	save := func(t *testing.T, body map[string]any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("project_uuid", w.project)
		rc.URLParams.Add("field_id", field.ID.String())
		rec := httptest.NewRecorder()
		taskFieldController.UpdateField(rec, httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(raw)).WithContext(context.WithValue(asAna, chi.RouteCtxKey, rc)))
		return rec.Code, rec.Body.String()
	}
	value := func(t *testing.T) string {
		t.Helper()
		v, err := fieldModel.ValueOf(ctx, uuid.MustParse(w.task), field.ID)
		if err != nil {
			t.Fatal(err)
		}
		return string(v)
	}
	options := func(t *testing.T) []string {
		t.Helper()
		f, err := fieldModel.Get(ctx, project, field.ID)
		if err != nil {
			t.Fatal(err)
		}
		var labels []string
		for _, o := range f.Options {
			labels = append(labels, o.Label)
		}
		return labels
	}

	t.Run("a save from the first tab keeps Blocked and the task set to it", func(t *testing.T) {
		code, body := save(t, map[string]any{"name": "Stage", "type": "select", "options": []map[string]string{
			{"id": read[0].ID, "label": "Todo"}, {"id": read[1].ID, "label": "In progress"}}})
		if code != http.StatusOK {
			t.Fatalf("renaming Doing from the first tab: %d %s", code, body)
		}
		if got := options(t); len(got) != 3 || got[1] != "In progress" || got[2] != "Blocked" {
			t.Errorf("the field's options are %v, want Todo, In progress and Blocked", got)
		}
		if got := value(t); got != `"`+blocked+`"` {
			t.Errorf("the task set to Blocked is now %s", got)
		}
	})

	t.Run("a new option can't take the name of one the field keeps", func(t *testing.T) {
		code, body := save(t, map[string]any{"name": "Stage", "type": "select", "options": []map[string]string{
			{"id": read[0].ID, "label": "Todo"}, {"label": "blocked"}}})
		if code != http.StatusBadRequest {
			t.Errorf("a new option called blocked beside Blocked: %d %s, want 400", code, body)
		}
		if got := value(t); got != `"`+blocked+`"` {
			t.Errorf("the refused save changed the task's value to %s", got)
		}
	})

	t.Run("an option named in removed_options comes off the field and its tasks", func(t *testing.T) {
		code, body := save(t, map[string]any{"name": "Stage", "type": "select", "options": []map[string]string{
			{"id": read[0].ID, "label": "Todo"}, {"id": read[1].ID, "label": "In progress"}}, "removed_options": []string{blocked}})
		if code != http.StatusOK {
			t.Fatalf("taking Blocked out: %d %s", code, body)
		}
		if got := options(t); len(got) != 2 {
			t.Errorf("the field's options are %v, want Todo and In progress", got)
		}
		if got := value(t); got != "" {
			t.Errorf("the task kept the value %s of an option taken out", got)
		}
	})
}
