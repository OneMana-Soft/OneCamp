//go:build integration
// +build integration

package integration_test

// A project's releases over HTTP (P10-1b, jira-04): Ana, an admin, makes one
// and puts tasks in it; only the project's live tasks she may see go in.
// Ben, a member, reads it without the restricted task kept from him, and may
// change nothing. Fay, an admin, sees that task too. Cy, outside the project,
// finds nothing. A name is taken among live releases only; a delete is soft
// and a restore brings the release back with its tasks. Taking a task out is
// soft too: its row stays, marked, out of every read, count and most, and
// putting the task back brings that row back; a task ana may not see there
// can't be taken out. The list comes a page at a time, by its cursor, in
// its order through ties too (newest made first, then by id). Once
// the project is archived its releases read as before and change no more.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestAProjectsReleasesAreReadAsTheReaderAndChangedByItsAdmins$' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	releaseController "github.com/akashc777/OneCamp/controllers/Release"
	restoreController "github.com/akashc777/OneCamp/controllers/Restore"
	"github.com/akashc777/OneCamp/helpers"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
)

func TestAProjectsReleasesAreReadAsTheReaderAndChangedByItsAdmins(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	ana := w.account("ana")
	task, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, ana.UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	// w.task is kept to the project's admins and its maker: not to ben.
	if err := workAccess.Restrict(ctx, task, ana.UserPostgresInfo.Id, nil, time.Now()); err != nil {
		t.Fatalf("restrict: %v", err)
	}
	// A task of another project ana is in.
	otherProject, otherTask := uuid.NewString(), uuid.NewString()
	w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Other', $2, $3)`, otherProject, w.team, w.people["ana"])
	w.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, otherTask, otherProject, w.people["ana"])
	anaUID := ana.UserDgraphInfo.Uid
	w.dg.Mutate(t, []map[string]any{{"uid": "_:other", "dgraph.type": "Project", "project_uuid": otherProject, "project_name": "Other",
		"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": []map[string]any{{"uid": anaUID}}, "project_admins": []map[string]any{{"uid": anaUID}},
		"project_tasks": []map[string]any{{"uid": "_:ot", "dgraph.type": "Task", "task_uuid": otherTask, "task_name": "Elsewhere", "task_status": "todo",
			"task_deleted_at": "0001-01-01T00:00:00Z", "task_project": map[string]any{"uid": "_:other"}, "task_created_by": map[string]any{"uid": anaUID}}}}})

	callAt := func(who, method, target string, h http.HandlerFunc, params map[string]string, body any) (int, string) {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(method, target, bytes.NewReader(raw)).WithContext(c))
		return rec.Code, compact(rec.Body.Bytes())
	}
	call := func(who, method string, h http.HandlerFunc, params map[string]string, body any) (int, string) {
		t.Helper()
		return callAt(who, method, "/", h, params, body)
	}
	inProject := map[string]string{"project_uuid": w.project}
	at := func(release string, more ...string) map[string]string {
		p := map[string]string{"project_uuid": w.project, "release_id": release}
		for i := 0; i+1 < len(more); i += 2 {
			p[more[i]] = more[i+1]
		}
		return p
	}
	idOf := func(body string) string {
		var out struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		return out.Data.ID
	}
	type view struct {
		Data struct {
			Progress struct{ Total, Done int } `json:"progress"`
			Tasks    []struct {
				UUID string `json:"task_uuid"`
			} `json:"tasks"`
			CanEdit bool   `json:"can_edit"`
			State   string `json:"state"`
		} `json:"data"`
	}
	read := func(who, id string) (int, view) {
		t.Helper()
		code, body := call(who, http.MethodGet, releaseController.GetRelease, at(id), nil)
		var v view
		_ = json.Unmarshal([]byte(body), &v)
		return code, v
	}

	code, body := call("ana", http.MethodPost, releaseController.CreateRelease, inProject, map[string]any{"name": "v1.0", "target_date": "2026-12-01"})
	rel := idOf(body)
	if code != http.StatusOK || rel == "" {
		t.Fatalf("ana makes a release: %d %s", code, body)
	}

	t.Run("only the project's live tasks ana may see go in", func(t *testing.T) {
		code, body := call("ana", http.MethodPost, releaseController.AddReleaseTasks, at(rel),
			map[string]any{"task_uuids": []string{w.task, w.assigned, w.deleted, otherTask, "not-a-task"}})
		if code != http.StatusOK {
			t.Fatalf("add: %d %s", code, body)
		}
		var out struct {
			Data struct {
				Added   []string `json:"added"`
				Refused []struct {
					Task string `json:"task_uuid"`
				} `json:"refused"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		if len(out.Data.Added) != 2 || len(out.Data.Refused) != 3 {
			t.Errorf("added %v refused %v, want the two live tasks in, the deleted, the other project's and the non-task refused", out.Data.Added, out.Data.Refused)
		}
	})

	t.Run("ben reads it without the task kept from him, and changes nothing", func(t *testing.T) {
		code, v := read("ben", rel)
		if code != http.StatusOK || v.Data.Progress.Total != 1 || len(v.Data.Tasks) != 1 || v.Data.Tasks[0].UUID != w.assigned || v.Data.CanEdit {
			t.Errorf("ben reads: %d %+v", code, v)
		}
		code, body := call("ben", http.MethodGet, releaseController.ListReleases, inProject, nil)
		var list struct {
			Data struct {
				Releases []struct {
					Progress struct{ Total int } `json:"progress"`
				} `json:"releases"`
				CanEdit bool `json:"can_edit"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &list)
		if code != http.StatusOK || strings.Contains(body, w.task) || len(list.Data.Releases) != 1 || list.Data.Releases[0].Progress.Total != 1 || list.Data.CanEdit {
			t.Errorf("ben lists: %d %s", code, body)
		}
		for name, h := range map[string]http.HandlerFunc{
			"create": releaseController.CreateRelease, "edit": releaseController.EditRelease, "ship": releaseController.ShipRelease,
			"add": releaseController.AddReleaseTasks, "remove": releaseController.RemoveReleaseTask, "delete": releaseController.DeleteRelease,
		} {
			if code, body := call("ben", http.MethodPost, h, at(rel, "task_uuid", w.assigned), map[string]any{"name": "mine", "task_uuids": []string{w.mine}}); code != http.StatusForbidden {
				t.Errorf("ben's %s: %d %s", name, code, body)
			}
		}
	})

	t.Run("fay, an admin, sees every task; cy, outside the project, finds nothing", func(t *testing.T) {
		if code, v := read("fay", rel); code != http.StatusOK || v.Data.Progress.Total != 2 || !v.Data.CanEdit {
			t.Errorf("fay reads: %d %+v", code, v)
		}
		if code, body := call("cy", http.MethodGet, releaseController.ListReleases, inProject, nil); code != http.StatusNotFound {
			t.Errorf("cy lists: %d %s", code, body)
		}
		if code, _ := read("cy", rel); code != http.StatusNotFound {
			t.Errorf("cy reads: %d", code)
		}
	})

	var again string // a second v1.0, deleted
	t.Run("a name is taken among live releases only; a delete is soft and a restore brings the tasks back", func(t *testing.T) {
		if code, body := call("ana", http.MethodPost, releaseController.CreateRelease, inProject, map[string]any{"name": "V1.0"}); code != http.StatusBadRequest {
			t.Errorf("a second V1.0: %d %s", code, body)
		}
		if code, body := call("ana", http.MethodPost, releaseController.DeleteRelease, at(rel), nil); code != http.StatusOK {
			t.Fatalf("delete: %d %s", code, body)
		}
		if code, _ := read("fay", rel); code != http.StatusNotFound {
			t.Errorf("a deleted release reads: %d", code)
		}
		var kept int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM release_tasks WHERE release_id = $1`, rel).Scan(&kept); err != nil || kept != 2 {
			t.Errorf("a deleted release kept %d tasks: %v", kept, err)
		}
		code, body := call("ana", http.MethodPost, releaseController.CreateRelease, inProject, map[string]any{"name": "v1.0"})
		again = idOf(body)
		if code != http.StatusOK || again == "" {
			t.Fatalf("the name is free once deleted: %d %s", code, body)
		}
		restore := map[string]string{"kind": "release", "id": rel}
		if code, body := call("ana", http.MethodPost, restoreController.Restore, restore, nil); code != http.StatusBadRequest {
			t.Errorf("a restore while v1.0 lives again: %d %s", code, body)
		}
		if code, body := call("ana", http.MethodPost, releaseController.DeleteRelease, at(again), nil); code != http.StatusOK {
			t.Fatalf("delete the second: %d %s", code, body)
		}
		// A put-in that read the release before its delete: not found, and
		// nothing written, never "already" for every task.
		if added, err := releaseModel.AddTasks(ctx, uuid.MustParse(again), []uuid.UUID{uuid.MustParse(w.mine)}, releaseModel.Manual, w.people["ana"], 500); !errors.Is(err, helpers.ErrNotFound) || len(added) != 0 {
			t.Errorf("a put-in into a release deleted since: %v %v, want not found", added, err)
		}
		var into int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM release_tasks WHERE release_id = $1`, again).Scan(&into); err != nil || into != 0 {
			t.Errorf("a deleted release took %d tasks: %v", into, err)
		}
		if code, body := call("ben", http.MethodPost, restoreController.Restore, restore, nil); code != http.StatusForbidden {
			t.Errorf("ben restores: %d %s", code, body)
		}
		if code, body := call("ana", http.MethodPost, restoreController.Restore, restore, nil); code != http.StatusOK {
			t.Fatalf("restore: %d %s", code, body)
		}
		if code, v := read("fay", rel); code != http.StatusOK || v.Data.Progress.Total != 2 {
			t.Errorf("restored with its tasks: %d %+v", code, v)
		}
	})

	t.Run("ana ships it and takes a task out: its row stays, marked, and counts nowhere", func(t *testing.T) {
		if code, body := call("ana", http.MethodPost, releaseController.ShipRelease, at(rel), nil); code != http.StatusOK {
			t.Errorf("ship: %d %s", code, body)
		}
		if code, body := call("ana", http.MethodPost, releaseController.RemoveReleaseTask, at(rel, "task_uuid", w.assigned), nil); code != http.StatusOK {
			t.Errorf("remove: %d %s", code, body)
		}
		var marked bool
		var by uuid.NullUUID
		if err := w.env.PG.QueryRow(`SELECT deleted_at IS NOT NULL, deleted_by FROM release_tasks WHERE release_id = $1 AND task_uuid = $2`, rel, w.assigned).Scan(&marked, &by); err != nil || !marked || by.UUID != w.people["ana"] {
			t.Errorf("the task taken out is kept, deleted by ana: marked %v by %v, %v", marked, by, err)
		}
		code, v := read("fay", rel)
		if code != http.StatusOK || v.Data.State != "released" || v.Data.Progress.Total != 1 || v.Data.Progress.Done != 0 || len(v.Data.Tasks) != 1 || v.Data.Tasks[0].UUID != w.task {
			t.Errorf("shipped, without the task: %d %+v", code, v)
		}
		if code, v := read("ben", rel); code != http.StatusOK || v.Data.Progress.Total != 0 || len(v.Data.Tasks) != 0 {
			t.Errorf("ben, who saw only that task, reads none: %d %+v", code, v)
		}
		if code, body := call("ana", http.MethodPost, releaseController.RemoveReleaseTask, at(rel, "task_uuid", w.assigned), nil); code != http.StatusNotFound {
			t.Errorf("taken out twice: %d %s", code, body)
		}
	})

	relID, anaID := uuid.MustParse(rel), w.people["ana"]
	rowOf := func(task string) (rows int, live bool, addedBy, source string) {
		t.Helper()
		if err := w.env.PG.QueryRow(`SELECT count(*), COALESCE(bool_and(deleted_at IS NULL AND deleted_by IS NULL), false),
			COALESCE(max(added_by::text), ''), COALESCE(max(source), '') FROM release_tasks WHERE release_id = $1 AND task_uuid = $2`,
			rel, task).Scan(&rows, &live, &addedBy, &source); err != nil {
			t.Fatal(err)
		}
		return
	}

	// The two SQL halves of a put-in, each on its own: the INSERT of a task
	// new to the release, and the ON CONFLICT revive of one taken out.
	t.Run("a task new to the release goes in as one row of its own (the INSERT)", func(t *testing.T) {
		added, err := releaseModel.AddTasks(ctx, relID, []uuid.UUID{uuid.MustParse(w.mine), uuid.MustParse(w.mine)}, releaseModel.Manual, anaID, 500)
		if err != nil || len(added) != 1 || added[0] != w.mine {
			t.Errorf("put in twice in one call: %v %v, want it once", added, err)
		}
		if rows, live, by, source := rowOf(w.mine); rows != 1 || !live || by != anaID.String() || source != releaseModel.Manual {
			t.Errorf("one live row, by ana, by hand: %d %v %s %s", rows, live, by, source)
		}
	})

	t.Run("a task already in stays as it joined", func(t *testing.T) {
		code, body := call("fay", http.MethodPost, releaseController.AddReleaseTasks, at(rel), map[string]any{"task_uuids": []string{w.task}})
		if code != http.StatusOK || !strings.Contains(body, `"already":["`+w.task+`"]`) {
			t.Errorf("already in: %d %s", code, body)
		}
		if rows, live, by, _ := rowOf(w.task); rows != 1 || !live || by != anaID.String() {
			t.Errorf("still ana's: %d %v %s", rows, live, by)
		}
	})

	t.Run("the most counts live tasks only, and past it nothing is written", func(t *testing.T) {
		// Two live tasks (task, mine) and one taken out (assigned).
		if added, err := releaseModel.AddTasks(ctx, relID, []uuid.UUID{uuid.MustParse(otherTask)}, releaseModel.Manual, anaID, 2); !errors.Is(err, releaseModel.ErrFull) || len(added) != 0 {
			t.Errorf("a third live task past a most of two: %v %v, want ErrFull", added, err)
		}
		if rows, _, _, _ := rowOf(otherTask); rows != 0 {
			t.Errorf("past the most %d rows were written", rows)
		}
		added, err := releaseModel.AddTasks(ctx, relID, []uuid.UUID{uuid.MustParse(otherTask)}, releaseModel.Manual, anaID, 3)
		if err != nil || len(added) != 1 {
			t.Errorf("a most of three, two live and one taken out, took %v: %v", added, err)
		}
	})

	t.Run("a task in the release that isn't one of the project's tasks ana sees can't be taken out", func(t *testing.T) {
		// otherTask went in through the model above: it is another project's.
		if code, body := call("ana", http.MethodPost, releaseController.RemoveReleaseTask, at(rel, "task_uuid", otherTask), nil); code != http.StatusNotFound {
			t.Errorf("take out a task ana may not see here: %d %s, want 404", code, body)
		}
		if rows, live, _, _ := rowOf(otherTask); rows != 1 || !live {
			t.Errorf("its row is still in: %d %v", rows, live)
		}
	})

	t.Run("putting a task back brings its row back (the ON CONFLICT revive)", func(t *testing.T) {
		code, body := call("fay", http.MethodPost, releaseController.AddReleaseTasks, at(rel), map[string]any{"task_uuids": []string{w.assigned}})
		if code != http.StatusOK || !strings.Contains(body, `"added":["`+w.assigned+`"]`) {
			t.Errorf("put back: %d %s", code, body)
		}
		if rows, live, by, _ := rowOf(w.assigned); rows != 1 || !live || by != w.people["fay"].String() {
			t.Errorf("one row, live again, put back by fay: %d %v %s", rows, live, by)
		}
		code, v := read("fay", rel)
		back := false
		for _, task := range v.Data.Tasks {
			back = back || task.UUID == w.assigned
		}
		if code != http.StatusOK || !back || v.Data.Progress.Total != len(v.Data.Tasks) {
			t.Errorf("fay reads it back: %d %+v", code, v)
		}
	})

	t.Run("the list comes a page at a time, in its order, by its cursor", func(t *testing.T) {
		for _, in := range []map[string]any{{"name": "v1.1", "target_date": "2026-12-20"}, {"name": "v1.2"}} {
			if code, body := call("ana", http.MethodPost, releaseController.CreateRelease, inProject, in); code != http.StatusOK {
				t.Fatalf("make %v: %d %s", in["name"], code, body)
			}
		}
		page := func(query string) (int, []string, string) {
			t.Helper()
			code, body := callAt("fay", http.MethodGet, "/?"+query, releaseController.ListReleases, inProject, nil)
			var out struct {
				Data struct {
					Releases []struct {
						Name string `json:"name"`
					} `json:"releases"`
					Next *string `json:"next_cursor"`
				} `json:"data"`
			}
			_ = json.Unmarshal([]byte(body), &out)
			var names []string
			for _, r := range out.Data.Releases {
				names = append(names, r.Name)
			}
			if code == http.StatusOK && out.Data.Next == nil {
				t.Errorf("a page without next_cursor: %s", body)
				return code, names, ""
			}
			if out.Data.Next == nil {
				return code, names, ""
			}
			return code, names, *out.Data.Next
		}
		// walk reads the list limit a page, by its cursors, in at most most pages.
		walk := func(limit, most int) ([]string, int) {
			t.Helper()
			var got []string
			cursor, pages := "", 0
			for {
				code, names, next := page(fmt.Sprintf("limit=%d&cursor=%s", limit, cursor))
				pages++
				if code != http.StatusOK || len(names) > limit || pages > most {
					t.Fatalf("page %d: %d %v", pages, code, names)
				}
				got = append(got, names...)
				if next == "" {
					return got, pages
				}
				cursor = next
			}
		}
		// Planned before out, the soonest target first, none last: v1.1
		// (20 Dec), v1.2 (no day), then v1.0 (shipped).
		got, pages := walk(1, 4)
		if strings.Join(got, ",") != "v1.1,v1.2,v1.0" || pages != 3 {
			t.Errorf("a page at a time read %v in %d pages, want v1.1,v1.2,v1.0 in 3", got, pages)
		}
		if code, names, next := page("limit=2"); code != http.StatusOK || strings.Join(names, ",") != "v1.1,v1.2" || next == "" {
			t.Errorf("two a page: %d %v %q", code, names, next)
		}
		if code, names, next := page(""); code != http.StatusOK || len(names) != 3 || next != "" {
			t.Errorf("a page unasked holds all three: %d %v %q", code, names, next)
		}
		if code, _, _ := page("cursor=WEB-1"); code != http.StatusBadRequest {
			t.Errorf("a cursor no list gave: %d, want 400", code)
		}
		// Ties, each across a page boundary (a page of one): v1.3 and v1.4,
		// planned with no day like v1.2, page newest made first (the falling
		// pair); v1.5 and v1.6, set made at the same moment by SQL, then page
		// by id. Both are newer than v1.4.
		ids := map[string]string{}
		for _, name := range []string{"v1.3", "v1.4", "v1.5", "v1.6"} {
			code, body := call("ana", http.MethodPost, releaseController.CreateRelease, inProject, map[string]any{"name": name})
			if ids[name] = idOf(body); code != http.StatusOK || ids[name] == "" {
				t.Fatalf("make %s: %d %s", name, code, body)
			}
		}
		w.exec(`UPDATE releases SET created_at = (SELECT created_at FROM releases WHERE id = $2) WHERE id = $1`, ids["v1.5"], ids["v1.6"])
		tied := []string{"v1.5", "v1.6"}
		if ids["v1.6"] < ids["v1.5"] {
			tied = []string{"v1.6", "v1.5"}
		}
		want := append(append([]string{"v1.1"}, tied...), "v1.4", "v1.3", "v1.2", "v1.0")
		if got, pages := walk(1, 8); strings.Join(got, ",") != strings.Join(want, ",") || pages != 7 {
			t.Errorf("with ties, a page at a time read %v in %d pages, want %v in 7", got, pages, want)
		}
	})

	t.Run("once the project is archived its releases read as before, and change no more", func(t *testing.T) {
		_, before := read("fay", rel)
		if code, body := call("ana", http.MethodPost, projectController.ArchiveProject, nil, map[string]string{"project_uuid": w.project}); code != http.StatusOK {
			t.Fatalf("archive: %d %s", code, body)
		}
		code, v := read("fay", rel)
		if code != http.StatusOK || v.Data.CanEdit || v.Data.Progress.Total != before.Data.Progress.Total || len(v.Data.Tasks) != len(before.Data.Tasks) {
			t.Errorf("fay reads an archived project's release: %d %+v, want it as before without can_edit (%+v)", code, v, before)
		}
		if code, v := read("ben", rel); code != http.StatusOK || v.Data.CanEdit || strings.Contains(fmt.Sprint(v.Data.Tasks), w.task) {
			t.Errorf("ben reads it, still without the task kept from him: %d %+v", code, v)
		}
		code, body := call("fay", http.MethodGet, releaseController.ListReleases, inProject, nil)
		if code != http.StatusOK || !strings.Contains(body, `"can_edit":false`) || strings.Contains(body, `"can_edit":true`) || !strings.Contains(body, `"v1.1"`) {
			t.Errorf("fay lists an archived project's releases: %d %s", code, body)
		}
		if code, _ := read("cy", rel); code != http.StatusNotFound {
			t.Errorf("cy, outside it, still finds nothing: %d", code)
		}
		for name, h := range map[string]http.HandlerFunc{
			"create": releaseController.CreateRelease, "edit": releaseController.EditRelease, "ship": releaseController.ShipRelease,
			"add": releaseController.AddReleaseTasks, "remove": releaseController.RemoveReleaseTask, "delete": releaseController.DeleteRelease,
		} {
			code, body := call("fay", http.MethodPost, h, at(rel, "task_uuid", w.task), map[string]any{"name": "v9", "task_uuids": []string{w.mine}})
			if code != http.StatusForbidden || !strings.Contains(body, "archived") {
				t.Errorf("fay's %s in an archived project: %d %s, want 403 in words", name, code, body)
			}
		}
		if code, body := call("ana", http.MethodPost, restoreController.Restore, map[string]string{"kind": "release", "id": again}, nil); code != http.StatusForbidden || !strings.Contains(body, "archived") {
			t.Errorf("a restore in an archived project: %d %s, want 403 in words", code, body)
		}
		if _, after := read("fay", rel); after.Data.Progress.Total != before.Data.Progress.Total || after.Data.State != before.Data.State {
			t.Errorf("a refused change changed the release: %+v, was %+v", after, before)
		}
	})
}
