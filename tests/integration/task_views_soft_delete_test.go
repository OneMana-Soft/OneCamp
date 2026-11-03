//go:build integration
// +build integration

// A saved task view its person deletes is kept, with who deleted it and when
// (V-184 head 5a, the owner's rule that every delete of a business record is
// soft).
//
// Deleting a view DELETEd its row. Now the row stays, marked by the person
// who deleted it, and no read sees it: their list of views, the name check
// and the count that hold the 30-view limit, and the access rules a saved
// view is read through. Saving the name again makes a new view, and the one
// deleted stays as it was deleted.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestASavedViewDeletedIsKeptWithWhoAndWhen|TestTaskViewsSoftDeleteMigrationUpDownUp' -v
package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestASavedViewDeletedIsKeptWithWhoAndWhen(t *testing.T) {
	ctx := context.Background()
	w := newDocsWorld(t, []string{"ada", "maya"}, "ada")
	as := func(name string) *userModels.UserInfo {
		info := userModels.UserInfo{}
		info.UserPostgresInfo.Id = w.people[name]
		info.UserPostgresInfo.EmailID = w.people[name].String() + "@example.test"
		info.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: w.uids[name], Uuid: w.people[name].String(), UserName: name}
		return &info
	}
	asAda, asMaya := as("ada"), as("maya")
	scope := "project:" + uuid.NewString()

	type view struct {
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		State json.RawMessage `json:"state"`
	}
	answer := func(t *testing.T, h http.HandlerFunc, method, path string, who *userModels.UserInfo, body string) (int, []byte) {
		t.Helper()
		rec := call(t, h, method, path, who, body, nil)
		return rec.Code, rec.Body.Bytes()
	}
	save := func(t *testing.T, who *userModels.UserInfo, name, state string) view {
		t.Helper()
		code, b := answer(t, taskController.SaveTaskView, http.MethodPost, "/task/views", who,
			fmt.Sprintf(`{"scope":%q,"name":%q,"state":%s}`, scope, name, state))
		if code != http.StatusOK {
			t.Fatalf("saving %q answered %d: %s", name, code, b)
		}
		var got struct {
			Data view `json:"data"`
		}
		if err := json.Unmarshal(b, &got); err != nil || got.Data.ID == "" {
			t.Fatalf("saving %q: %s (%v)", name, b, err)
		}
		return got.Data
	}
	del := func(t *testing.T, who *userModels.UserInfo, id string) int {
		t.Helper()
		code, _ := answer(t, taskController.DeleteTaskView, http.MethodPost, "/task/views/delete", who, fmt.Sprintf(`{"id":%q}`, id))
		return code
	}
	list := func(t *testing.T, who *userModels.UserInfo) []view {
		t.Helper()
		code, b := answer(t, taskController.GetTaskViews, http.MethodGet, "/task/views?scope="+url.QueryEscape(scope), who, "")
		if code != http.StatusOK {
			t.Fatalf("listing answered %d: %s", code, b)
		}
		var got struct {
			Data []view `json:"data"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		return got.Data
	}
	listed := func(views []view, id string) bool {
		for _, v := range views {
			if v.ID == id {
				return true
			}
		}
		return false
	}
	// takenBy is who deleted the view, for a row that is kept, marked: the
	// zero id while it is live. A row that is gone fails the test.
	takenBy := func(t *testing.T, id string) uuid.UUID {
		t.Helper()
		var at sql.NullTime
		var by uuid.NullUUID
		if err := w.env.PG.QueryRow(`SELECT deleted_at, deleted_by FROM task_views WHERE id = $1`, id).Scan(&at, &by); err != nil {
			t.Fatalf("view %s: %v; want it kept, marked by who deleted it", id, err)
		}
		if at.Valid != by.Valid {
			t.Fatalf("view %s: deleted_at %v but deleted_by %v: a view deleted says when and by whom", id, at, by)
		}
		return by.UUID
	}
	level := func(t *testing.T, who, id string) accessBusiness.Level {
		t.Helper()
		d, err := accessBusiness.Rules.Resolve(ctx, w.subject(who), accessBusiness.Resource{Kind: workAccess.KindView, UUID: id})
		if err != nil {
			t.Fatal(err)
		}
		return d.Level
	}

	t.Run("a view deleted is kept, marked by the person who deleted it, and nothing reads it", func(t *testing.T) {
		mine := save(t, asAda, "Mine", `{"sort":"due"}`)
		if !listed(list(t, asAda), mine.ID) || level(t, "ada", mine.ID) != accessBusiness.LevelFull {
			t.Fatalf("setup: Ada's new view should be listed and hers in full")
		}
		if code := del(t, asMaya, mine.ID); code != http.StatusNotFound {
			t.Fatalf("Maya deleting Ada's view answered %d; want 404, as it isn't hers", code)
		}
		if by := takenBy(t, mine.ID); by != uuid.Nil {
			t.Fatalf("Maya's refused delete marked Ada's view, by %s", by)
		}
		if code := del(t, asAda, mine.ID); code != http.StatusOK {
			t.Fatalf("Ada deleting her view answered %d", code)
		}
		if by := takenBy(t, mine.ID); by != w.people["ada"] {
			t.Fatalf("Ada's deleted view is marked by %s; want Ada %s", by, w.people["ada"])
		}
		if listed(list(t, asAda), mine.ID) {
			t.Fatalf("Ada's list still shows the view she deleted")
		}
		if l := level(t, "ada", mine.ID); l != accessBusiness.LevelNone {
			t.Fatalf("the access rules still give Ada %v on the view she deleted; want none", l)
		}
		if code := del(t, asAda, mine.ID); code != http.StatusNotFound {
			t.Fatalf("deleting the view again answered %d; want 404", code)
		}

		// Saved again under its name, it's a new view; the deleted one stays
		// as it was deleted.
		again := save(t, asAda, "Mine", `{"sort":"title"}`)
		if again.ID == mine.ID {
			t.Fatalf("saving the deleted view's name again brought the deleted row back (%s); want a new view", mine.ID)
		}
		var state string
		if err := w.env.PG.QueryRow(`SELECT state::text FROM task_views WHERE id = $1`, mine.ID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if by := takenBy(t, mine.ID); by != w.people["ada"] || state != `{"sort": "due"}` {
			t.Fatalf("the deleted view changed when its name was saved again: by %s, state %s", by, state)
		}
		if views := list(t, asAda); len(views) != 1 || views[0].ID != again.ID {
			t.Fatalf("Ada's list = %+v; want only the view saved again", views)
		}
	})

	t.Run("the 30-view limit counts live views only, and a deleted view's name is free to count", func(t *testing.T) {
		if _, err := w.env.PG.Exec(`UPDATE task_views SET deleted_at = NOW(), deleted_by = $1 WHERE user_id = $1 AND scope = $2 AND deleted_at IS NULL`, w.people["ada"], scope); err != nil {
			t.Fatal(err)
		}
		views := make([]view, 0, 30)
		for i := range 30 {
			views = append(views, save(t, asAda, fmt.Sprintf("View %02d", i), `{}`))
		}
		if code, b := answer(t, taskController.SaveTaskView, http.MethodPost, "/task/views", asAda,
			fmt.Sprintf(`{"scope":%q,"name":"One more","state":{}}`, scope)); code == http.StatusOK {
			t.Fatalf("a 31st view was saved (%s); want the limit to refuse it", b)
		}
		// "Mine" was deleted above: its name is no live view's, so saving it is
		// a new view the limit has to count, not a replacement it lets through.
		if code, b := answer(t, taskController.SaveTaskView, http.MethodPost, "/task/views", asAda,
			fmt.Sprintf(`{"scope":%q,"name":"Mine","state":{}}`, scope)); code == http.StatusOK {
			t.Fatalf("saving a deleted view's name at the limit went through (%s); want it refused as a 31st view", b)
		}
		if code := del(t, asAda, views[0].ID); code != http.StatusOK {
			t.Fatalf("deleting a view answered %d", code)
		}
		save(t, asAda, "One more", `{}`) // a view deleted frees its place
		if got := len(list(t, asAda)); got != 30 {
			t.Fatalf("Ada lists %d views; want 30", got)
		}
	})
}

func TestTaskViewsSoftDeleteMigrationUpDownUp(t *testing.T) {
	// The head's four files, in the order they go up.
	names := []string{"task_views_soft_delete", "task_views_name_live_idx", "task_views_user_id_idx", "task_views_unique_while_live"}
	read := func(name, dir string) string {
		t.Helper()
		files, err := filepath.Glob("../../migrations/*_" + name + "." + dir + ".sql")
		if err != nil || len(files) != 1 {
			t.Fatalf("migration %s's %s file: %v %v", name, dir, files, err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var up, down []string
	for i := range names {
		up = append(up, read(names[i], "up"))
		down = append(down, read(names[len(names)-1-i], "down"))
	}
	env := integration.SetupEnv(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	sam := uuid.New()
	exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'sam@example.test', 'sam', 'Sam', NOW(), NOW())`, sam)
	// One view live and one deleted under the same name, which only the
	// partial unique allows.
	exec(`INSERT INTO task_views (user_id, scope, name, state, deleted_at, deleted_by) VALUES ($1, 'mine', 'Week', '{"kept":true}', NULL, NULL), ($1, 'mine', 'Week', '{"kept":false}', NOW(), $1)`, sam)
	counts := func() (live, marked int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*) FILTER (WHERE state->>'kept' = 'true'), count(*) FILTER (WHERE state->>'kept' = 'false') FROM task_views WHERE user_id = $1`, sam).Scan(&live, &marked); err != nil {
			t.Fatal(err)
		}
		return live, marked
	}
	fullUnique := func() bool {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conname = 'task_views_user_id_scope_name_key'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if fullUnique() {
		t.Fatal("at head, the full unique on (user_id, scope, name) is still there")
	}

	for _, q := range down {
		exec(q)
	}
	if live, marked := counts(); live != 1 || marked != 0 {
		t.Fatalf("after the downs: %d live, %d marked; want the live view kept and the deleted one gone", live, marked)
	}
	if !fullUnique() {
		t.Fatal("after the downs, the full unique on (user_id, scope, name) isn't back")
	}
	var cols int
	if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'task_views' AND column_name IN ('deleted_at', 'deleted_by')`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("after the downs task_views has %d of its mark columns (%v); want none", cols, err)
	}

	for _, q := range up {
		exec(q)
	}
	if fullUnique() {
		t.Fatal("after the ups again, the full unique on (user_id, scope, name) is still there")
	}
	exec(`INSERT INTO task_views (user_id, scope, name, state, deleted_at, deleted_by) VALUES ($1, 'mine', 'Week', '{"kept":false}', NOW(), $1)`, sam)
	if _, err := env.PG.Exec(`INSERT INTO task_views (user_id, scope, name, state) VALUES ($1, 'mine', 'Week', '{}')`, sam); err == nil {
		t.Fatal("after the ups again, a second live view named Week went in; want the partial unique to refuse it")
	}
	if live, marked := counts(); live != 1 || marked != 1 {
		t.Fatalf("after the ups again: %d live, %d marked; want 1 and 1", live, marked)
	}
}
