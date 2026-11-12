//go:build integration
// +build integration

package integration_test

// Projects B9-b1 against real Postgres 12 and Dgraph, through the report's
// handler: insights open on any view of a project's list or board, because
// the report takes that view's filters exactly as the list sends them and
// runs them through the list's own builder (domain/Task.TaskListFilter), and
// work slices by a project's custom select, multi-select and person fields.
// Who sees what: the report reads only the reader's live projects, so a
// field's values reach only the people who read its project, and a filter
// never widens that.
//
// Run: go test -tags=integration ./tests/integration/ -run TestB9b1 -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	taskFieldBusiness "github.com/akashc777/OneCamp/business/TaskField"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	"github.com/akashc777/OneCamp/helpers"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
)

func TestB9b1InsightsOnAnyViewAndFieldSlices(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	project := uuid.MustParse(w.project)

	// The report's root is the reader's projects: ben and ana list Website.
	// The live tasks are dated an hour ago, so each has an age: the launch
	// post and the newsletter are high, the copy check low.
	var got struct {
		P []struct {
			UID string `json:"uid"`
		} `json:"p"`
		T []struct {
			UID  string `json:"uid"`
			UUID string `json:"task_uuid"`
		} `json:"t"`
	}
	raw := w.dg.Query(t, fmt.Sprintf(`{ p(func: eq(project_uuid, %q)) { uid } t(func: type(Task)) @filter(eq(task_uuid, [%q, %q, %q])) { uid task_uuid } }`,
		w.project, w.task, w.assigned, w.mine))
	if err := json.Unmarshal(raw, &got); err != nil || len(got.P) != 1 || len(got.T) != 3 {
		t.Fatalf("Website and its live tasks in the graph: %s, %v", raw, err)
	}
	made := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	prio := map[string]string{w.task: "high", w.assigned: "low", w.mine: "high"}
	mut := []map[string]any{
		{"uid": w.account("ben").UserDgraphInfo.Uid, "user_projects": []map[string]any{{"uid": got.P[0].UID}}},
		{"uid": w.account("ana").UserDgraphInfo.Uid, "user_projects": []map[string]any{{"uid": got.P[0].UID}}},
	}
	for _, n := range got.T {
		mut = append(mut, map[string]any{"uid": n.UID, "task_created_at": made, "task_priority": prio[n.UUID]})
	}
	w.dg.Mutate(t, mut)

	// Website's fields: Team (select), Areas (multi-select), Reviewer
	// (person) and Notes (text, which slices by its values since L32).
	field := func(in taskFieldBusiness.Input) *taskFieldModel.Field {
		t.Helper()
		f, err := taskFieldBusiness.Create(ctx, project, in, w.people["ana"], "")
		if err != nil {
			t.Fatalf("field %s: %v", in.Name, err)
		}
		return f
	}
	team := field(taskFieldBusiness.Input{Name: "Team", Type: taskFieldBusiness.TypeSelect,
		Options: []taskFieldBusiness.OptionInput{{Label: "Web", Color: "blue"}, {Label: "Ops", Color: "green"}}})
	areas := field(taskFieldBusiness.Input{Name: "Areas", Type: taskFieldBusiness.TypeMultiSelect,
		Options: []taskFieldBusiness.OptionInput{{Label: "Docs", Color: "blue"}, {Label: "Site", Color: "green"}}})
	reviewer := field(taskFieldBusiness.Input{Name: "Reviewer", Type: taskFieldBusiness.TypePerson})
	notes := field(taskFieldBusiness.Input{Name: "Notes", Type: taskFieldBusiness.TypeText})
	web, ops := team.Options[0].ID, team.Options[1].ID
	docs, site := areas.Options[0].ID, areas.Options[1].ID
	set := func(task string, f *taskFieldModel.Field, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if err := taskFieldModel.SetValue(ctx, uuid.MustParse(task), f.ID, b, w.people["ana"]); err != nil {
			t.Fatalf("%s on %s: %v", f.Name, task, err)
		}
	}
	// The launch post: Web, Docs + Site, reviewed by ben. The newsletter:
	// Ops, Docs. The copy check: no values at all.
	set(w.task, team, web)
	set(w.task, areas, []string{docs, site})
	set(w.task, reviewer, w.people["ben"].String())
	set(w.task, notes, "draft")
	set(w.mine, team, ops)
	set(w.mine, areas, []string{docs})

	type spread struct {
		N int `json:"n"`
	}
	type slice struct {
		By   string `json:"by"`
		Key  string `json:"key"`
		Name string `json:"name"`
		Age  spread `json:"age"`
	}
	type report struct {
		Data struct {
			Insights struct {
				Age    spread  `json:"age"`
				Slices []slice `json:"slices"`
				Fields []struct {
					By      string `json:"by"`
					Name    string `json:"name"`
					Type    string `json:"type"`
					Project string `json:"project_uuid"`
				} `json:"fields"`
			} `json:"insights"`
		} `json:"data"`
	}
	read := func(who, query string) (int, report, string) {
		t.Helper()
		c := context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who))
		rec := httptest.NewRecorder()
		projectController.Report(rec, httptest.NewRequest(http.MethodGet, "/project/report?"+query, http.NoBody).WithContext(c))
		var r report
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return rec.Code, r, rec.Body.String()
	}
	filters := func(f ...map[string]any) string {
		b, _ := json.Marshal(f)
		return "&filters=" + url.QueryEscape(string(b))
	}
	ageIn := func(r report, by, key string) (int, bool) {
		for _, s := range r.Data.Insights.Slices {
			if s.By == by && s.Key == key {
				return s.Age.N, true
			}
		}
		return 0, false
	}
	byTeam, byAreas, byReviewer := "field:"+team.ID.String(), "field:"+areas.ID.String(), "field:"+reviewer.ID.String()
	only := "projects=" + w.project

	t.Run("work slices by a project's select, multi-select and person fields", func(t *testing.T) {
		code, r, body := read("ben", only)
		if code != http.StatusOK || r.Data.Insights.Age.N != 3 {
			t.Fatalf("ben's report of Website: %d, age %+v: %s", code, r.Data.Insights.Age, body)
		}
		for _, c := range []struct {
			by, key string
			n       int
		}{
			{byTeam, web, 1}, {byTeam, ops, 1}, {byTeam, "", 1},
			{byAreas, docs, 2}, {byAreas, site, 1}, {byAreas, "", 1},
			{byReviewer, w.people["ben"].String(), 1}, {byReviewer, "", 2},
		} {
			if n, ok := ageIn(r, c.by, c.key); !ok || n != c.n {
				t.Errorf("slice %s %q: %d open (found %v), want %d: %s", c.by, c.key, n, ok, c.n, body)
			}
		}
		names := map[string]string{}
		for _, s := range r.Data.Insights.Slices {
			names[s.By+"/"+s.Key] = s.Name
		}
		if names[byTeam+"/"+web] != "Web" || names[byAreas+"/"+site] != "Site" || names[byReviewer+"/"+w.people["ben"].String()] != "ben" {
			t.Errorf("slices name their option or person: %v", names)
		}
		// The fields sliced, named for the picker.
		want := map[string]string{byTeam: "Team/select", byAreas: "Areas/multi_select", byReviewer: "Reviewer/person", "field:" + notes.ID.String(): "Notes/text"}
		if len(r.Data.Insights.Fields) != len(want) {
			t.Fatalf("fields %+v, want Team, Areas, Reviewer and Notes", r.Data.Insights.Fields)
		}
		for _, f := range r.Data.Insights.Fields {
			if want[f.By] != f.Name+"/"+f.Type || f.Project != w.project {
				t.Errorf("field %+v, want %s in Website", f, want[f.By])
			}
		}
		if n, ok := ageIn(r, "field:"+notes.ID.String(), "draft"); !ok || n != 1 {
			t.Errorf("a text field slices by its value: %s", body)
		}
	})

	t.Run("insights take a view's filters as its list sends them", func(t *testing.T) {
		for _, c := range []struct {
			what  string
			query string
			n     int
		}{
			{"high priority", filters(map[string]any{"id": "task_priority", "value": []string{"high"}}), 2},
			{"Team is Ops (a custom field, through the list's own side filter)", filters(map[string]any{"id": taskFieldBusiness.FilterID(team.ID), "value": []string{ops}}), 1},
			{"Team is set, and high", filters(map[string]any{"id": taskFieldBusiness.FilterID(team.ID), "value": []string{taskFieldBusiness.FilterAny}},
				map[string]any{"id": "task_priority", "value": []string{"high"}}), 2},
			{"assigned to eve", filters(map[string]any{"id": "task_assignee_name", "value": []string{w.account("eve").UserDgraphInfo.Uid}}), 1},
			{"the search box", "&taskSearchString=newsletter", 1},
		} {
			code, r, body := read("ben", only+c.query)
			if code != http.StatusOK || r.Data.Insights.Age.N != c.n {
				t.Errorf("%s: %d, %d open, want %d: %s", c.what, code, r.Data.Insights.Age.N, c.n, body)
			}
		}
		// The slices follow the filter: only Ops' task is left in Team.
		_, r, body := read("ben", only+filters(map[string]any{"id": taskFieldBusiness.FilterID(team.ID), "value": []string{ops}}))
		if _, ok := ageIn(r, byTeam, web); ok {
			t.Errorf("Team is Ops still slices Web: %s", body)
		}
		code, _, body := read("ben", only+"&filters=%5Bnot-json")
		if code != http.StatusBadRequest {
			t.Errorf("filters that aren't JSON: %d %s, want 400", code, body)
		}
	})

	t.Run("nobody reads a project's fields or work through a report they can't read", func(t *testing.T) {
		// cy is in the team, not the project: asking for Website by name,
		// with its field as a filter, reads nothing of it.
		code, r, body := read("cy", only+filters(map[string]any{"id": taskFieldBusiness.FilterID(team.ID), "value": []string{taskFieldBusiness.FilterAny}}))
		if code != http.StatusOK || r.Data.Insights.Age.N != 0 || len(r.Data.Insights.Fields) != 0 {
			t.Fatalf("cy's report: %d %s; want nothing of Website", code, body)
		}
		for _, s := range r.Data.Insights.Slices {
			if s.Name == "Web" || s.Name == "Ops" || s.Name == "Website" {
				t.Errorf("cy sees %+v", s)
			}
		}
	})
}
