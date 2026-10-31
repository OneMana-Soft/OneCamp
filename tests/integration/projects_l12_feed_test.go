//go:build integration

package integration_test

// linear-12 part 4: a project's cycles as a calendar, by a person's secret
// link (business/Cycle/feed.go). Anyone who may view the project makes one;
// someone outside it is refused. The link answers the cycles while its person
// may view the project, and nothing once a newer link replaces it, it is
// stopped, its person leaves the workspace or the project is archived.
// Run: integ_rel.sh <wt> <log> 'TestLinear12DACycleFeedFollowsItsPersonsAccess'

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	cycleController "github.com/akashc777/OneCamp/controllers/Cycle"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestLinear12DACycleFeedFollowsItsPersonsAccess(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	s := newWA2Scale(t, env, dg)
	s.addTasks(6) // todo, inProgress, inReview, backlog, done, canceled

	// Cy is in the workspace but not in the project.
	s.person("cy", false)

	// The project's running cycle, with four tasks, one of them done.
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	cycle := uuid.New()
	s.exec(`INSERT INTO project_cycles (id, project_uuid, number, name, starts_at, ends_at) VALUES ($1, $2, 1, '', $3, $4)`, cycle, s.project, day.AddDate(0, 0, -3), day.AddDate(0, 0, 11))
	for _, i := range []int{0, 1, 2, 4} {
		s.exec(`INSERT INTO task_cycles (task_uuid, cycle_id) VALUES ($1, $2)`, s.tasks[i], cycle)
	}

	makeLink := func(who string) (int, string) {
		t.Helper()
		code, out := s.postAs(who, cycleController.MakeCycleFeed, map[string]any{})
		if code != http.StatusOK {
			return code, out
		}
		var got struct {
			Data struct {
				Live bool   `json:"live"`
				URL  string `json:"url"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil || !got.Data.Live || !strings.Contains(got.Data.URL, "/public/cycles/") || !strings.HasSuffix(got.Data.URL, ".ics") {
			t.Fatalf("the link as made: %s", out)
		}
		return code, got.Data.URL[strings.LastIndex(got.Data.URL, "/")+1:]
	}
	read := func(file string) (int, string) {
		t.Helper()
		rc := chi.NewRouteContext()
		rc.URLParams.Add("file", file)
		rec := httptest.NewRecorder()
		cycleController.PublicCycleFeed(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rc)))
		return rec.Code, rec.Body.String()
	}

	// Someone outside the project can't make one.
	if code, out := makeLink("cy"); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("someone outside the project made a link: %d %s", code, out)
	}

	// Ben, a member, makes one; it reads the project's cycles.
	code, first := makeLink("ben")
	if code != http.StatusOK {
		t.Fatalf("Ben's link: %d %s", code, first)
	}
	code, ics := read(first)
	if code != http.StatusOK || !strings.Contains(ics, "BEGIN:VCALENDAR") || !strings.Contains(ics, "SUMMARY:Big board: Cycle 1") ||
		!strings.Contains(ics, "1 of 4 tasks done") || !strings.Contains(ics, "DTSTART;VALUE=DATE:"+day.AddDate(0, 0, -3).Format("20060102")) {
		t.Fatalf("Ben's link reads: %d\n%s", code, ics)
	}
	// Reading it says nothing of whether it's live: GET answers without the address.
	if code, out := s.read("ben", cycleController.CycleFeed, ""); code != http.StatusOK || !strings.Contains(out, `"live": true`) || strings.Contains(out, first) {
		t.Fatalf("Ben's link as read back: %d %s", code, out)
	}

	// A new link replaces the old one at once; a made-up one reads nothing.
	_, second := makeLink("ben")
	if code, _ := read(first); code != http.StatusNotFound {
		t.Fatalf("the replaced link still answers: %d", code)
	}
	if code, _ := read("not-a-link.ics"); code != http.StatusNotFound {
		t.Fatalf("a made-up link answers: %d", code)
	}
	if code, _ := read(second); code != http.StatusOK {
		t.Fatalf("the new link: %d", code)
	}

	// Stopped, it answers nothing.
	if code, out := s.postAs("ben", cycleController.StopCycleFeed, map[string]any{}); code != http.StatusNoContent {
		t.Fatalf("stopping Ben's link: %d %s", code, out)
	}
	if code, _ := read(second); code != http.StatusNotFound {
		t.Fatalf("the stopped link still answers: %d", code)
	}

	// Ana's link stops answering once the project is archived; Ben's once he leaves the workspace.
	_, ana := makeLink("ana")
	_, third := makeLink("ben")
	dg.Mutate(t, []map[string]any{{"uid": s.uids["ben"], "user_deleted_at": now.Format(time.RFC3339)}})
	if code, _ := read(third); code != http.StatusNotFound {
		t.Fatalf("the link of someone who left still answers: %d", code)
	}
	if code, _ := read(ana); code != http.StatusOK {
		t.Fatalf("Ana's link: %d", code)
	}
	dg.Mutate(t, []map[string]any{{"uid": s.uids["p"], "project_deleted_at": now.Format(time.RFC3339)}})
	if code, _ := read(ana); code != http.StatusNotFound {
		t.Fatalf("the link to an archived project still answers: %d", code)
	}
}
