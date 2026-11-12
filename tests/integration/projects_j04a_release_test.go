//go:build integration
// +build integration

package integration_test

// jira-04 head A over HTTP: a release's burndown, counted as its reader sees
// it (a restricted task kept from ben is never counted for him); the release
// field on every task, set by whoever may change the task, a release already
// out by the project's admins only, and the task list filter on releases,
// "No release" too; Mark released moving the unfinished tasks on.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestJ04A' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	releaseController "github.com/akashc777/OneCamp/controllers/Release"
	"github.com/akashc777/OneCamp/helpers"
)

// j04a is the work world with w.task kept from ben (restricted to the
// project's admins and its maker), and another project ana runs.
type j04a struct {
	*workWorld
	ctx                     context.Context
	otherProject, otherTask string
}

func seedJ04A(t *testing.T) *j04a {
	w := &j04a{workWorld: seedWorkWorld(t), ctx: context.Background()}
	ana := w.account("ana")
	task, err := taskBusiness.GetDgraphBasicTaskInfo(w.ctx, w.task, ana.UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := workAccess.Restrict(w.ctx, task, ana.UserPostgresInfo.Id, nil, time.Now()); err != nil {
		t.Fatalf("restrict: %v", err)
	}
	w.otherProject, w.otherTask = uuid.NewString(), uuid.NewString()
	w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Other', $2, $3)`, w.otherProject, w.team, w.people["ana"])
	w.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, w.otherTask, w.otherProject, w.people["ana"])
	anaUID := ana.UserDgraphInfo.Uid
	w.dg.Mutate(t, []map[string]any{{"uid": "_:other", "dgraph.type": "Project", "project_uuid": w.otherProject, "project_name": "Other",
		"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": []map[string]any{{"uid": anaUID}}, "project_admins": []map[string]any{{"uid": anaUID}},
		"project_tasks": []map[string]any{{"uid": "_:ot", "dgraph.type": "Task", "task_uuid": w.otherTask, "task_name": "Elsewhere", "task_status": "todo",
			"task_deleted_at": "0001-01-01T00:00:00Z", "task_project": map[string]any{"uid": "_:other"}, "task_created_by": map[string]any{"uid": anaUID}}}}})
	return w
}

// call runs handler h as who, with the URL's params and a JSON body.
func (w *j04a) call(who, method, target string, h http.HandlerFunc, params map[string]string, body any) (int, string) {
	w.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	c := context.WithValue(context.WithValue(w.ctx, helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, target, bytes.NewReader(raw)).WithContext(c))
	return rec.Code, compact(rec.Body.Bytes())
}

// release makes a release in project as ana, with tasks, and answers its id.
func (w *j04a) release(project, name string, target *string, tasks ...string) string {
	w.t.Helper()
	code, body := w.call("ana", http.MethodPost, "/", releaseController.CreateRelease, map[string]string{"project_uuid": project},
		map[string]any{"name": name, "target_date": target})
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || code != http.StatusOK || out.Data.ID == "" {
		w.t.Fatalf("ana makes %s: %d %s", name, code, body)
	}
	if len(tasks) > 0 {
		code, body = w.call("ana", http.MethodPost, "/", releaseController.AddReleaseTasks, w.at(project, out.Data.ID), map[string]any{"task_uuids": tasks})
		if code != http.StatusOK {
			w.t.Fatalf("ana fills %s: %d %s", name, code, body)
		}
	}
	return out.Data.ID
}

func (w *j04a) at(project, release string) map[string]string {
	return map[string]string{"project_uuid": project, "release_id": release}
}

// set puts fields on a task in the graph.
func (w *j04a) set(task string, fields map[string]any) {
	w.t.Helper()
	var got struct {
		T []struct {
			UID string `json:"uid"`
		} `json:"t"`
	}
	raw := w.dg.Query(w.t, `{ t(func: eq(task_uuid, "`+task+`")) { uid } }`)
	if err := json.Unmarshal(raw, &got); err != nil || len(got.T) != 1 {
		w.t.Fatalf("task %s in the graph: %s %v", task, raw, err)
	}
	fields["uid"] = got.T[0].UID
	w.dg.Mutate(w.t, []map[string]any{fields})
}

type j04aBurndown struct {
	Data struct {
		Release struct {
			ID string `json:"id"`
		} `json:"release"`
		Burndown struct {
			Days      []string  `json:"days"`
			Scope     []int     `json:"scope"`
			Remaining []int     `json:"remaining"`
			Ideal     []float64 `json:"ideal"`
			Tasks     int       `json:"tasks"`
			Done      int       `json:"done"`
			Open      int       `json:"open"`
			Hours     float64   `json:"hours"`
		} `json:"burndown"`
	} `json:"data"`
}

func (w *j04a) burndown(who, project, release string) (int, j04aBurndown, string) {
	w.t.Helper()
	code, body := w.call(who, http.MethodGet, "/?tz=UTC", releaseController.ReleaseBurndown, w.at(project, release), nil)
	var b j04aBurndown
	_ = json.Unmarshal([]byte(body), &b)
	return code, b, body
}

func TestJ04AReleaseBurndownCountsWhatTheReaderSees(t *testing.T) {
	w := seedJ04A(t)
	day := func(d int) string { return time.Now().UTC().AddDate(0, 0, d).Format(time.DateOnly) }
	target := day(4)
	rel := w.release(w.project, "v2", &target, w.task, w.mine, w.assigned)
	// In since three days ago; the one assigned to ben was done yesterday.
	w.exec(`UPDATE release_tasks SET added_at = NOW() - interval '3 days' WHERE release_id = $1`, rel)
	w.set(w.mine, map[string]any{"task_status": "done", "task_status_since": time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339)})
	w.set(w.task, map[string]any{"task_estimate_minutes": 120})
	w.set(w.assigned, map[string]any{"task_estimate_minutes": 60})

	t.Run("ana sees every task, from the first joining to the target day", func(t *testing.T) {
		code, b, body := w.burndown("ana", w.project, rel)
		bd := b.Data.Burndown
		if code != http.StatusOK || b.Data.Release.ID != rel || len(bd.Days) != 8 || bd.Days[0] != day(-3) || bd.Days[7] != target {
			t.Fatalf("ana: %d %.400s", code, body)
		}
		if bd.Tasks != 3 || bd.Done != 1 || bd.Open != 2 || bd.Hours != 3 || len(bd.Remaining) != 4 ||
			fmt.Sprint(bd.Scope) != "[3 3 3 3]" || fmt.Sprint(bd.Remaining) != "[3 3 2 2]" || len(bd.Ideal) != 8 || bd.Ideal[7] != 0 {
			t.Errorf("ana's counts: %+v", bd)
		}
	})

	t.Run("ben never counts the task kept from him", func(t *testing.T) {
		code, b, body := w.burndown("ben", w.project, rel)
		bd := b.Data.Burndown
		if code != http.StatusOK || bd.Tasks != 2 || bd.Hours != 1 || fmt.Sprint(bd.Remaining) != "[2 2 1 1]" {
			t.Errorf("ben: %d %.400s", code, body)
		}
	})

	t.Run("outside the project, or through another project, there is none", func(t *testing.T) {
		if code, _, body := w.burndown("cy", w.project, rel); code != http.StatusNotFound {
			t.Errorf("cy: %d %s", code, body)
		}
		elsewhere := w.release(w.otherProject, "x1", nil, w.otherTask)
		if code, _, body := w.burndown("ana", w.project, elsewhere); code != http.StatusNotFound {
			t.Errorf("another project's release through this one: %d %s", code, body)
		}
	})

	t.Run("no target day: to today, and no ideal line", func(t *testing.T) {
		open := w.release(w.project, "someday", nil, w.assigned)
		w.exec(`UPDATE release_tasks SET added_at = NOW() - interval '2 days' WHERE release_id = $1`, open)
		code, b, body := w.burndown("ana", w.project, open)
		bd := b.Data.Burndown
		if code != http.StatusOK || len(bd.Days) != 3 || bd.Days[2] != day(0) || bd.Ideal != nil || fmt.Sprint(bd.Remaining) != "[1 1 1]" {
			t.Errorf("no target: %d %.400s", code, body)
		}
	})
}

type j04aField struct {
	Data struct {
		Releases []struct {
			ID string `json:"id"`
		} `json:"releases"`
		Options []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"options"`
		CanEdit           bool `json:"can_edit"`
		CanChangeReleased bool `json:"can_change_released"`
	} `json:"data"`
}

// ids is the field's releases, in its order.
func (f j04aField) ids() []string {
	var out []string
	for _, r := range f.Data.Releases {
		out = append(out, r.ID)
	}
	return out
}

func TestJ04ATaskReleaseFieldIsTheTasksEditorsAndTheFilterFindsIt(t *testing.T) {
	w := seedJ04A(t)
	target := time.Now().UTC().AddDate(0, 0, 14).Format(time.DateOnly)
	v1 := w.release(w.project, "v1", &target)
	v0 := w.release(w.project, "v0", nil)
	if code, body := w.call("ana", http.MethodPost, "/", releaseController.ShipRelease, w.at(w.project, v0), nil); code != http.StatusOK {
		t.Fatalf("ana ships v0: %d %s", code, body)
	}
	x1 := w.release(w.otherProject, "x1", nil)
	field := func(who, task string) (int, j04aField, string) {
		t.Helper()
		code, body := w.call(who, http.MethodGet, "/", releaseController.GetTaskReleases, map[string]string{"task_uuid": task}, nil)
		var f j04aField
		_ = json.Unmarshal([]byte(body), &f)
		return code, f, body
	}
	set := func(who, task string, ids ...string) (int, string) {
		t.Helper()
		return w.call(who, http.MethodPost, "/", releaseController.SetTaskReleases, nil, map[string]any{"task_uuid": task, "release_ids": append([]string{}, ids...)})
	}
	in := func(task string) []string {
		t.Helper()
		_, f, _ := field("ana", task)
		return f.ids()
	}

	t.Run("ben, who may change his task, puts it in a planned release, and its list has it at once", func(t *testing.T) {
		if code, body := set("ben", w.mine, v1); code != http.StatusOK || !strings.Contains(body, v1) {
			t.Fatalf("ben sets v1: %d %s", code, body)
		}
		code, body := w.call("ana", http.MethodGet, "/", releaseController.GetRelease, w.at(w.project, v1), nil)
		if code != http.StatusOK || !strings.Contains(body, `"task_uuid":"`+w.mine+`"`) || !strings.Contains(body, `"source":"manual"`) {
			t.Errorf("v1's tasks: %d %s", code, body)
		}
		code, f, body := field("ben", w.mine)
		options := fmt.Sprint(f.Data.Options)
		if code != http.StatusOK || fmt.Sprint(f.ids()) != "["+v1+"]" || !f.Data.CanEdit || f.Data.CanChangeReleased ||
			!strings.Contains(options, v1) || !strings.Contains(options, v0) || strings.Contains(options, x1) {
			t.Errorf("ben's field: %d %s", code, body)
		}
	})

	t.Run("a release already out changes only by the project's admins", func(t *testing.T) {
		if code, body := set("ben", w.mine, v1, v0); code != http.StatusForbidden || !strings.Contains(body, "admins") {
			t.Errorf("ben puts his task in v0, out already: %d %s, want 403 in words", code, body)
		}
		if code, body := set("ana", w.mine, v1, v0); code != http.StatusOK || len(in(w.mine)) != 2 {
			t.Fatalf("ana puts it in both: %d %s", code, body)
		}
		if code, body := set("ben", w.mine, v0); code != http.StatusOK || fmt.Sprint(in(w.mine)) != "["+v0+"]" {
			t.Errorf("ben takes it out of planned v1: %d %s", code, body)
		}
		var soft bool
		if err := w.env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM release_tasks WHERE release_id = $1 AND task_uuid = $2`, v1, w.mine).Scan(&soft); err != nil || !soft {
			t.Errorf("taking it out is soft: the row stays, marked (%v, %v)", soft, err)
		}
		if code, body := set("ben", w.mine); code != http.StatusForbidden || fmt.Sprint(in(w.mine)) != "["+v0+"]" {
			t.Errorf("ben takes it out of v0: %d %s, want 403 and it still in", code, body)
		}
	})

	t.Run("an id that isn't a live release of the task's own project is refused in words", func(t *testing.T) {
		gone := w.release(w.project, "gone", nil)
		if code, body := w.call("ana", http.MethodPost, "/", releaseController.DeleteRelease, w.at(w.project, gone), nil); code != http.StatusOK {
			t.Fatalf("delete: %d %s", code, body)
		}
		for name, id := range map[string]string{"another project's": x1, "a deleted": gone, "no": "not-a-release"} {
			if code, body := set("ana", w.mine, v0, id); code != http.StatusBadRequest || !strings.Contains(body, "release") {
				t.Errorf("%s release: %d %s, want 400 in words", name, code, body)
			}
		}
		if fmt.Sprint(in(w.mine)) != "["+v0+"]" {
			t.Errorf("a refused set changes nothing: %v", in(w.mine))
		}
	})

	t.Run("a task kept from ben, or outside cy's projects, isn't there", func(t *testing.T) {
		if code, body := set("ben", w.task, v1); code != http.StatusNotFound {
			t.Errorf("ben sets the task kept from him: %d %s", code, body)
		}
		if code, _, body := field("ben", w.task); code != http.StatusNotFound {
			t.Errorf("ben reads its field: %d %s", code, body)
		}
		if code, _, body := field("cy", w.mine); code != http.StatusNotFound {
			t.Errorf("cy reads a field in a project not his: %d %s", code, body)
		}
	})

	t.Run("the list, the board and its column filter by release, and by no release", func(t *testing.T) {
		readers := []struct {
			name, target string
			h            http.HandlerFunc
		}{
			{"list", "/?pageSize=50&pageIndex=0&filters=", projectController.GetProjectTaskList},
			{"kanban", "/?filters=", projectController.GetProjectTaskListForKanban},
			{"board column", "/?status=todo&filters=", projectController.GetProjectBoardColumn},
		}
		for _, choice := range []struct {
			values   []string
			has, not []string
		}{
			{[]string{"none"}, []string{w.task, w.assigned}, []string{w.mine}},
			{[]string{v0}, []string{w.mine}, []string{w.task, w.assigned}},
			{[]string{"none", v0}, []string{w.task, w.assigned, w.mine}, nil},
			{[]string{x1}, nil, []string{w.task, w.assigned, w.mine}},
		} {
			raw, _ := json.Marshal([]map[string]any{{"id": "task_release", "value": choice.values}})
			for _, r := range readers {
				code, body := w.call("ana", http.MethodGet, r.target+url.QueryEscape(string(raw)), r.h, map[string]string{"project_uuid": w.project}, nil)
				for _, id := range choice.has {
					if code != http.StatusOK || !strings.Contains(body, id) {
						t.Errorf("%s by %v: %d, want %s in %.300s", r.name, choice.values, code, id, body)
					}
				}
				for _, id := range choice.not {
					if strings.Contains(body, id) {
						t.Errorf("%s by %v: %s isn't in it", r.name, choice.values, id)
					}
				}
			}
		}
	})

	t.Run("with only admins changing tasks, ben reads his task's releases but sets none", func(t *testing.T) {
		if code, body := w.setEditWho("ana", "admins"); code != http.StatusOK {
			t.Fatalf("admins only: %d %s", code, body)
		}
		t.Cleanup(func() {
			if code, body := w.setEditWho("ana", nil); code != http.StatusOK {
				t.Errorf("back to the workspace's choice: %d %s", code, body)
			}
		})
		if code, f, body := field("ben", w.mine); code != http.StatusOK || f.Data.CanEdit || fmt.Sprint(f.ids()) != "["+v0+"]" {
			t.Errorf("ben's field, admins only: %d %s, want it read and not editable", code, body)
		}
		if code, body := set("ben", w.mine, v0, v1); code != http.StatusForbidden || !strings.Contains(body, "Only this project's admins change its tasks.") {
			t.Errorf("ben puts his task in planned v1, admins only: %d %s, want 403 in the task's words", code, body)
		}
		if got := fmt.Sprint(in(w.mine)); got != "["+v0+"]" {
			t.Errorf("the refused set changes nothing: %s", got)
		}
	})

	t.Run("an archived project's tasks change no releases", func(t *testing.T) {
		if code, body := w.call("ana", http.MethodPost, "/", projectController.ArchiveProject, nil, map[string]string{"project_uuid": w.project}); code != http.StatusOK {
			t.Fatalf("archive: %d %s", code, body)
		}
		if code, body := set("ana", w.mine, v0, v1); code != http.StatusForbidden || !strings.Contains(body, "archived") {
			t.Errorf("ana sets a release in an archived project: %d %s, want 403 in words", code, body)
		}
		if _, f, _ := field("ana", w.mine); f.Data.CanEdit || fmt.Sprint(f.ids()) != "["+v0+"]" {
			t.Errorf("an archived project's field reads as before, not editable: %+v", f.Data)
		}
	})
}

func TestJ04AMarkReleasedMovesUnfinishedTasks(t *testing.T) {
	w := seedJ04A(t)
	w.set(w.mine, map[string]any{"task_status": "done"})
	v3 := w.release(w.project, "v3", nil, w.task, w.mine, w.assigned)
	v4 := w.release(w.project, "v4", nil)
	out := w.release(w.project, "out", nil)
	x1 := w.release(w.otherProject, "x1", nil)
	ship := func(who, release string, body any) (int, string) {
		t.Helper()
		return w.call(who, http.MethodPost, "/", releaseController.ShipRelease, w.at(w.project, release), body)
	}
	if code, body := ship("ana", out, nil); code != http.StatusOK {
		t.Fatalf("ana ships out: %d %s", code, body)
	}
	type view struct {
		Data struct {
			State      string `json:"state"`
			Unfinished int    `json:"unfinished"`
			Tasks      []struct {
				UUID string `json:"task_uuid"`
			} `json:"tasks"`
		} `json:"data"`
	}
	read := func(release string) view {
		t.Helper()
		_, body := w.call("ana", http.MethodGet, "/", releaseController.GetRelease, w.at(w.project, release), nil)
		var v view
		_ = json.Unmarshal([]byte(body), &v)
		return v
	}
	has := func(release string, tasks ...string) bool {
		got := map[string]bool{}
		for _, x := range read(release).Data.Tasks {
			got[x.UUID] = true
		}
		for _, x := range tasks {
			if !got[x] {
				return false
			}
		}
		return len(got) == len(tasks)
	}

	t.Run("a release says how many of its tasks are unfinished", func(t *testing.T) {
		if v := read(v3); v.Data.Unfinished != 2 || v.Data.State != "planned" {
			t.Errorf("v3: %+v, want 2 unfinished", v.Data)
		}
	})

	t.Run("only the project's admins mark it released", func(t *testing.T) {
		if code, body := ship("ben", v3, map[string]any{"move_to": v4}); code != http.StatusForbidden {
			t.Errorf("ben: %d %s", code, body)
		}
	})

	t.Run("the tasks move only to a planned release of the same project", func(t *testing.T) {
		for name, to := range map[string]string{"another project's": x1, "one out already": out, "itself": v3, "no": "junk"} {
			if code, body := ship("ana", v3, map[string]any{"move_to": to}); code != http.StatusBadRequest || !strings.Contains(body, "planned release") {
				t.Errorf("to %s release: %d %s, want 400 in words", name, code, body)
			}
		}
		if v := read(v3); v.Data.State != "planned" || len(v.Data.Tasks) != 3 {
			t.Errorf("a refused move ships nothing: %+v", v.Data)
		}
	})

	t.Run("the unfinished tasks move to the planned release, the done one stays", func(t *testing.T) {
		code, body := ship("ana", v3, map[string]any{"move_to": v4})
		if code != http.StatusOK || !strings.Contains(body, `"moved":2`) {
			t.Fatalf("ana ships v3 into v4: %d %s", code, body)
		}
		if v := read(v3); v.Data.State != "released" || !has(v3, w.mine) || !has(v4, w.task, w.assigned) {
			t.Errorf("after: v3 %+v, v4 %+v", v.Data, read(v4).Data)
		}
		var soft int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM release_tasks t JOIN releases r ON r.id = t.release_id
			WHERE t.release_id = $1 AND t.deleted_at = r.released_at`, v3).Scan(&soft); err != nil || soft != 2 {
			t.Errorf("the moved rows stay, taken out as it went out: %d %v", soft, err)
		}
		// Its burndown still counts what it left unfinished, to the end.
		_, b, raw := w.burndown("ana", w.project, v3)
		bd := b.Data.Burndown
		if bd.Tasks != 3 || bd.Open != 2 || len(bd.Remaining) == 0 || bd.Remaining[len(bd.Remaining)-1] != 2 {
			t.Errorf("v3's burndown: %.400s", raw)
		}
	})

	t.Run("to none they just leave", func(t *testing.T) {
		v5 := w.release(w.project, "v5", nil, w.task, w.mine)
		if code, body := ship("ana", v5, map[string]any{"move_to": "none"}); code != http.StatusOK || !strings.Contains(body, `"moved":1`) || !has(v5, w.mine) {
			t.Errorf("ana ships v5, its unfinished leaving: %d %s %+v", code, body, read(v5).Data)
		}
	})

	t.Run("asked nothing, every task stays as before", func(t *testing.T) {
		v6 := w.release(w.project, "v6", nil, w.assigned)
		if code, body := ship("ana", v6, nil); code != http.StatusOK || !has(v6, w.assigned) || read(v6).Data.State != "released" {
			t.Errorf("ana ships v6: %d %s", code, body)
		}
	})
}
