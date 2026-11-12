//go:build integration
// +build integration

package integration_test

// V-698 against real Postgres 12 and Dgraph, through the handlers: a
// project's money fields are for its admins only. Fay is an admin of
// Website, ben a member. Both see the Budget field listed; fay sees its
// values everywhere a task's values are read, ben sees it hidden and none of
// its values anywhere: the field list, the task list, the board, the task
// panel, the CSV, a list filter, the report, the live messages and the Work
// Change Log. Setting a money value is fay's; ben hears 403 in words. A text
// field's values reach both as before.
//
// Run: go test -tags=integration ./tests/integration/ -run TestV698 -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskFieldController "github.com/akashc777/OneCamp/controllers/TaskField"
	workFeedController "github.com/akashc777/OneCamp/controllers/WorkFeed"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/akashc777/OneCamp/tests/integration"
)

// v698Field is the object an answer gives for the field id, wherever it sits
// (the field list, the panel's project_meta), or nil.
func v698Field(body []byte, id string) map[string]any {
	var v any
	_ = json.Unmarshal(body, &v)
	var found map[string]any
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			if x["id"] == id && x["type"] != nil && found == nil {
				found = x
				return
			}
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return found
}

func TestV698MoneyFieldsAreForProjectAdminsOnly(t *testing.T) {
	w := seedWorkWorld(t)
	broker := integration.RecordMqtt(t)
	ctx := context.Background()
	project := uuid.MustParse(w.project)

	// The report reads the reader's projects: fay and ben list Website.
	var got struct {
		P []struct {
			UID string `json:"uid"`
		} `json:"p"`
	}
	if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ p(func: eq(project_uuid, %q)) { uid } }`, w.project)), &got); err != nil || len(got.P) != 1 {
		t.Fatalf("Website in the graph: %+v, %v", got, err)
	}
	w.dg.Mutate(t, []map[string]any{
		{"uid": w.account("ben").UserDgraphInfo.Uid, "user_projects": []map[string]any{{"uid": got.P[0].UID}}},
		{"uid": w.account("fay").UserDgraphInfo.Uid, "user_projects": []map[string]any{{"uid": got.P[0].UID}}},
	})

	field := func(in taskFieldBusiness.Input) *taskFieldModel.Field {
		t.Helper()
		f, err := taskFieldBusiness.Create(ctx, project, in, w.people["ana"], "")
		if err != nil {
			t.Fatalf("field %s: %v", in.Name, err)
		}
		return f
	}
	budget := field(taskFieldBusiness.Input{Name: "Budget", Type: taskFieldBusiness.TypeMoney, Currency: "EUR"})
	notes := field(taskFieldBusiness.Input{Name: "Notes", Type: taskFieldBusiness.TypeText})
	set := func(task string, f *taskFieldModel.Field, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if err := taskFieldModel.SetValue(ctx, uuid.MustParse(task), f.ID, b, w.people["ana"]); err != nil {
			t.Fatalf("%s on %s: %v", f.Name, task, err)
		}
	}
	// The newsletter (assigned to ben): a budget of 9876.54 and a note.
	set(w.mine, budget, 987654)
	set(w.mine, notes, "kept for all")
	const money, shown, note = "987654", "9876.54", "kept for all"

	project1 := map[string]string{"project_uuid": w.project}
	get := func(t *testing.T, who string, h http.HandlerFunc, path string, params map[string]string) []byte {
		t.Helper()
		u := w.account(who)
		rec := call(t, h, http.MethodGet, path, &u, "", params)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s GET %s: %d %s", who, path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	// values: fay reads the budget's value, ben none of it; both read the note.
	values := func(t *testing.T, what string, fay, ben []byte) {
		t.Helper()
		if s := compact(fay); !strings.Contains(s, money) || !strings.Contains(s, note) {
			t.Errorf("fay's %s lacks the budget or the note: %.600s", what, s)
		}
		if s := compact(ben); strings.Contains(s, money) || !strings.Contains(s, note) {
			t.Errorf("ben's %s carries the budget, or lacks the note: %.600s", what, s)
		}
	}
	// hidden: both list Budget; ben's says hidden, fay's doesn't.
	hidden := func(t *testing.T, what string, fay, ben []byte) {
		t.Helper()
		f, b := v698Field(fay, budget.ID.String()), v698Field(ben, budget.ID.String())
		if f == nil || b == nil || f["name"] != "Budget" || b["name"] != "Budget" || b["type"] != "money" {
			t.Fatalf("Budget listed in %s: fay %v, ben %v", what, f, b)
		}
		if f["hidden"] == true || b["hidden"] != true {
			t.Errorf("Budget's hidden in %s: fay %v, ben %v", what, f["hidden"], b["hidden"])
		}
		if n := v698Field(ben, notes.ID.String()); n == nil || n["hidden"] == true {
			t.Errorf("Notes in ben's %s: %v", what, n)
		}
	}

	t.Run("fields", func(t *testing.T) {
		path := "/project/" + w.project + "/fields"
		hidden(t, "field list", get(t, "fay", taskFieldController.ListFields, path, project1), get(t, "ben", taskFieldController.ListFields, path, project1))
	})

	t.Run("list", func(t *testing.T) {
		path := "/project/taskList/" + w.project + "?getAll=true"
		values(t, "task list", get(t, "fay", projectController.GetProjectTaskList, path, project1), get(t, "ben", projectController.GetProjectTaskList, path, project1))
	})

	t.Run("board", func(t *testing.T) {
		path := "/project/taskListForKanban/" + w.project
		values(t, "board", get(t, "fay", projectController.GetProjectTaskListForKanban, path, project1),
			get(t, "ben", projectController.GetProjectTaskListForKanban, path, project1))
		path = "/project/boardColumn/" + w.project + "?status=todo"
		values(t, "board column", get(t, "fay", projectController.GetProjectBoardColumn, path, project1),
			get(t, "ben", projectController.GetProjectBoardColumn, path, project1))
	})

	t.Run("panel", func(t *testing.T) {
		path := "/task/info/" + w.mine + "?include=project_meta"
		params := map[string]string{"task_uuid": w.mine}
		fay, ben := get(t, "fay", taskController.GetTaskInfo, path, params), get(t, "ben", taskController.GetTaskInfo, path, params)
		values(t, "task panel", fay, ben)
		hidden(t, "task panel's project_meta", fay, ben)
	})

	t.Run("export", func(t *testing.T) {
		path := "/project/taskList/" + w.project + "?format=csv&tz=UTC"
		fay, ben := string(get(t, "fay", projectController.GetProjectTaskList, path, project1)), string(get(t, "ben", projectController.GetProjectTaskList, path, project1))
		if !strings.Contains(fay, "Budget (EUR)") || !strings.Contains(fay, shown) || !strings.Contains(fay, note) {
			t.Errorf("fay's CSV lacks the budget: %.600s", fay)
		}
		if strings.Contains(ben, "Budget") || strings.Contains(ben, shown) || !strings.Contains(ben, note) {
			t.Errorf("ben's CSV carries the budget: %.600s", ben)
		}
	})

	t.Run("filter", func(t *testing.T) {
		// A filter on the budget narrows fay's list to the newsletter; for
		// ben, who can't read it, it narrows nothing, so it tells him nothing.
		f, _ := json.Marshal([]map[string]any{{"id": taskFieldBusiness.FilterID(budget.ID), "value": []string{"any"}}})
		path := "/project/taskList/" + w.project + "?getAll=true&filters=" + url.QueryEscape(string(f))
		fay, ben := compact(get(t, "fay", projectController.GetProjectTaskList, path, project1)), compact(get(t, "ben", projectController.GetProjectTaskList, path, project1))
		if !strings.Contains(fay, w.mine) || strings.Contains(fay, w.task) {
			t.Errorf("fay's list with a budget: %.600s", fay)
		}
		if !strings.Contains(ben, w.mine) || !strings.Contains(ben, w.task) || !strings.Contains(ben, w.assigned) {
			t.Errorf("ben's list filtered on a budget he can't read: %.600s", ben)
		}
	})

	t.Run("report", func(t *testing.T) {
		// Insights already slice by money for admins only: it stays so.
		by := `"by":"field:` + budget.ID.String() + `"`
		path := "/project/report?projects=" + w.project
		fay, ben := compact(get(t, "fay", projectController.Report, path, nil)), compact(get(t, "ben", projectController.Report, path, nil))
		if !strings.Contains(fay, by) || strings.Contains(ben, by) || strings.Contains(ben, money) {
			t.Errorf("the report's budget slices: fay %v, ben %v", strings.Contains(fay, by), strings.Contains(ben, by))
		}
	})

	// The Work Change Log, from before the sets below, as ben reads it.
	var head struct {
		Data struct {
			Cursor string `json:"cursor"`
		} `json:"data"`
	}
	if err := json.Unmarshal(get(t, "ben", workFeedController.Changes, "/work/changes", nil), &head); err != nil || head.Data.Cursor == "" {
		t.Fatalf("the log's head: %+v, %v", head, err)
	}

	const fays = "123456"
	t.Run("set", func(t *testing.T) {
		post := func(who string, f *taskFieldModel.Field, value string) (int, string) {
			t.Helper()
			u := w.account(who)
			rec := call(t, taskFieldController.SetTaskField, http.MethodPost, "/task/field", &u,
				fmt.Sprintf(`{"task_uuid":%q,"field_id":%q,"value":%s}`, w.mine, f.ID, value), nil)
			return rec.Code, rec.Body.String()
		}
		if code, body := post("ben", budget, "5"); code != http.StatusForbidden || !strings.Contains(body, "Only project admins can set money fields.") {
			t.Errorf("ben setting the budget: %d %s", code, body)
		}
		if code, body := post("ben", notes, `"ben's note"`); code != http.StatusOK {
			t.Errorf("ben setting the note: %d %s", code, body)
		}
		if code, body := post("fay", budget, fays); code != http.StatusOK || !strings.Contains(compact([]byte(body)), `"value":`+fays) {
			t.Errorf("fay setting the budget: %d %s", code, body)
		}
		if v, _ := taskFieldModel.ValueOf(ctx, uuid.MustParse(w.mine), budget.ID); string(v) != fays {
			t.Errorf("the budget kept: %s", v)
		}
	})

	t.Run("mqtt", func(t *testing.T) {
		// Everyone in the project hears a message: a money field's carries
		// no value and asks each app to read the task again as itself.
		topic := helpers.GetMqttTopicForProjectMessage(w.project)
		var budgetTold, noteTold bool
		for _, m := range broker.Messages(topic, mqttStruct.MESSAGE_TASK_FIELD) {
			var msg struct {
				FieldID string          `json:"field_id"`
				Value   json.RawMessage `json:"value"`
				Refetch bool            `json:"refetch"`
			}
			_ = json.Unmarshal(m, &msg)
			switch msg.FieldID {
			case budget.ID.String():
				budgetTold = true
				if strings.Contains(string(m), fays) || !msg.Refetch {
					t.Errorf("the budget's message: %s", m)
				}
			case notes.ID.String():
				noteTold = true
				if !strings.Contains(string(msg.Value), "ben's note") || msg.Refetch {
					t.Errorf("the note's message: %s", m)
				}
			}
		}
		if !budgetTold || !noteTold {
			t.Fatalf("messages told: budget %v, note %v: %d published", budgetTold, noteTold, len(broker.All()))
		}
		for _, m := range broker.Messages(topic, mqttStruct.MESSAGE_TASK_CHANGED) {
			if strings.Contains(string(m), fays) {
				t.Errorf("a change message carries the budget: %s", m)
			}
		}
	})

	t.Run("work changes", func(t *testing.T) {
		body := compact(get(t, "ben", workFeedController.Changes, "/work/changes?cursor="+head.Data.Cursor, nil))
		if strings.Contains(body, fays) || !strings.Contains(body, budget.ID.String()) || !strings.Contains(body, "ben's note") {
			t.Errorf("ben's Work Change Log: %.800s", body)
		}
	})

	t.Run("kept log", func(t *testing.T) {
		// A change the log kept from before V-698 holds a budget's value:
		// migration 584 takes it off, as new ones are written, and leaves
		// the note's.
		old := `{"field_id":"` + budget.ID.String() + `","value":555444}`
		w.exec(`INSERT INTO task_changes (project_uuid, resource_uuid, kind, diff) VALUES ($1, $2, 'task.updated', $3), ($1, $2, 'task.updated', $4)`,
			w.project, w.mine, old, `{"field_id":"`+notes.ID.String()+`","value":"an old note"}`)
		up, err := os.ReadFile("../../migrations/584_task_changes_money_scrub.up.sql")
		if err != nil {
			t.Fatal(err)
		}
		w.exec(string(up))
		var valued, refetch, kept int
		if err := w.env.PG.QueryRow(`SELECT count(*) FILTER (WHERE diff::text LIKE '%555444%'), count(*) FILTER (WHERE diff = $2::jsonb),
			count(*) FILTER (WHERE diff ->> 'value' = 'an old note') FROM task_changes WHERE resource_uuid = $1`,
			w.mine, `{"field_id":"`+budget.ID.String()+`","refetch":true}`).Scan(&valued, &refetch, &kept); err != nil {
			t.Fatal(err)
		}
		if valued != 0 || refetch < 2 || kept != 1 {
			t.Errorf("the kept log after 584: %d money values, %d refetch changes, %d notes", valued, refetch, kept)
		}
	})
}
