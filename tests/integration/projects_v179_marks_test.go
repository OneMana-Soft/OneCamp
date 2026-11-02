//go:build integration
// +build integration

package integration_test

// V-179 (the owner's rule: every delete is soft) through the real callers. A
// take-off and a lift sent to the task restriction route, and a member taken
// off a project or a team, mark the rows by whoever did it. The model's own
// tests (models/postgres/TaskRestriction) hand it the who directly; these
// prove each caller passes on the person on the request.
// Website's admins are Ana and Fay, and Ben is its member; Dee is the admin of
// its team, Web (seedWorkWorld).
// Run: PKGS=./tests/integration/ integ_rel.sh <wt> <log> '^TestV179'

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskRestrictionController "github.com/akashc777/OneCamp/controllers/TaskRestriction"
	teamController "github.com/akashc777/OneCamp/controllers/Team"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// v179Call runs a handler as who, signed in as the auth middleware leaves
// them, with the route's parameters and a JSON body. It answers the status
// and the body.
func v179Call(t *testing.T, w *workWorld, who string, h http.HandlerFunc, params map[string]string, body any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	ctx, _ := w.signedIn(who)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc)))
	return rec.Code, rec.Body.String()
}

// v179Mark is a kept row's mark: when it was taken off, and by whom.
func v179Mark(t *testing.T, w *workWorld, q string, args ...any) (sql.NullTime, uuid.NullUUID) {
	t.Helper()
	var (
		at sql.NullTime
		by uuid.NullUUID
	)
	if err := w.env.PG.QueryRow(q, args...).Scan(&at, &by); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return at, by
}

const (
	v179GrantMark       = `SELECT deleted_at, deleted_by FROM task_restriction_grants WHERE task_id = $1 AND user_id = $2`
	v179RestrictionMark = `SELECT deleted_at, deleted_by FROM task_restrictions WHERE task_id = $1`
)

// v179LetBenIn has Ana, an admin of the project, let Ben into her task.
func v179LetBenIn(t *testing.T, w *workWorld) {
	t.Helper()
	if code, out := v179Call(t, w, "ana", taskRestrictionController.SetRestriction, map[string]string{"task_ref": w.task},
		map[string]any{"restricted": true, "people": []string{w.people["ben"].String()}}); code != http.StatusOK {
		t.Fatalf("Ana letting Ben into her task: %d %s", code, out)
	}
}

// Ana takes Ben off her task through the route, and Fay, the project's other
// admin, lifts the restriction: each mark names the admin who did it.
func TestV179TheRestrictionRouteMarksATakeOffAndALiftByTheAdmin(t *testing.T) {
	w := seedWorkWorld(t)
	v179LetBenIn(t, w)
	route := map[string]string{"task_ref": w.task}
	if code, out := v179Call(t, w, "ana", taskRestrictionController.SetRestriction, route,
		map[string]any{"restricted": true, "people": []string{}}); code != http.StatusOK {
		t.Fatalf("Ana taking Ben off: %d %s", code, out)
	}
	if at, by := v179Mark(t, w, v179GrantMark, w.task, w.people["ben"]); !at.Valid || by.UUID != w.people["ana"] {
		t.Fatalf("Ben's grant is marked %v by %v, want taken off by Ana", at, by)
	}
	if code, out := v179Call(t, w, "fay", taskRestrictionController.SetRestriction, route, map[string]any{"restricted": false}); code != http.StatusOK {
		t.Fatalf("Fay lifting the restriction: %d %s", code, out)
	}
	if at, by := v179Mark(t, w, v179RestrictionMark, w.task); !at.Valid || by.UUID != w.people["fay"] {
		t.Fatalf("the restriction is marked %v by %v, want lifted by Fay", at, by)
	}
}

// Fay takes Ben off the project, or Dee takes him off its team: either way his
// grant on the restricted task is marked by whoever took him off.
func TestV179TakingSomeoneOffAProjectOrItsTeamMarksTheirGrantByTheRemover(t *testing.T) {
	for _, c := range []struct {
		from, remover string
		remove        http.HandlerFunc
		body          func(w *workWorld) map[string]string
	}{
		{"project", "fay", projectController.RemoveMemberFromProject, func(w *workWorld) map[string]string {
			return map[string]string{"project_uuid": w.project, "user_uuid": w.people["ben"].String()}
		}},
		{"team", "dee", teamController.RemoveMemberFromTeam, func(w *workWorld) map[string]string {
			return map[string]string{"team_uuid": w.team, "member_uuid": w.people["ben"].String()}
		}},
	} {
		t.Run(c.from, func(t *testing.T) {
			w := seedWorkWorld(t)
			v179LetBenIn(t, w)
			if code, out := v179Call(t, w, c.remover, c.remove, nil, c.body(w)); code != http.StatusOK {
				t.Fatalf("%s taking Ben off the %s: %d %s", c.remover, c.from, code, out)
			}
			if at, by := v179Mark(t, w, v179GrantMark, w.task, w.people["ben"]); !at.Valid || by.UUID != w.people[c.remover] {
				t.Fatalf("Ben's grant is marked %v by %v, want by %s, who took him off the %s", at, by, c.remover, c.from)
			}
		})
	}
}
