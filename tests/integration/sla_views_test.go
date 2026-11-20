//go:build integration
// +build integration

package integration_test

// The SLA views (linear-24 H3) through the project's own routes: a member
// filters its task list by where the tasks' timers stand and sorts it by
// when they breach, and Insights slices its work by SLA, none of it showing
// a restricted task they can't read; a guest with a member's place is
// refused each (403) and their plain list is unchanged; with the project's
// SLAs off, the filter matches nothing and the sort is the list's own.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	slaBusiness "github.com/akashc777/OneCamp/business/SLA"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	taskRestrictionController "github.com/akashc777/OneCamp/controllers/TaskRestriction"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/tests/integration"
)

// slaGet runs a GET handler as who with the query q and the world's project
// as its project_uuid.
func slaGet(w *workWorld, who string, h http.HandlerFunc, q url.Values) (int, string) {
	w.t.Helper()
	rc := chi.NewRouteContext()
	rc.URLParams.Add("project_uuid", w.project)
	ctx := context.WithValue(context.WithValue(context.Background(), helpers.UserInfoContextKey, w.account(who)), chi.RouteCtxKey, rc)
	rec := httptest.NewRecorder()
	customMiddleware.RequestIDMiddleware(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?"+q.Encode(), nil).WithContext(ctx))
	return rec.Code, rec.Body.String()
}

// slaList is the project's task list as who, filtered and sorted as given
// ("" for none): its code and its tasks' uuids in order.
func slaList(w *workWorld, who, filters, sorting string) (int, []string, string) {
	w.t.Helper()
	q := url.Values{"pageSize": {"50"}, "pageIndex": {"0"}}
	if filters != "" {
		q.Set("filters", filters)
	}
	if sorting != "" {
		q.Set("sorting", sorting)
	}
	code, out := slaGet(w, who, projectController.GetProjectTaskList, q)
	var got struct {
		Data struct {
			Tasks []struct {
				UUID string `json:"task_uuid"`
			} `json:"project_tasks"`
		} `json:"data"`
	}
	var ids []string
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			w.t.Fatalf("the list as %s: %v in %s", who, err, out)
		}
		for _, x := range got.Data.Tasks {
			ids = append(ids, x.UUID)
		}
	}
	return code, ids, out
}

// slaSlices is the report's Insights slices by SLA as who, by key: open
// tasks; and their keys with their ranks, in the order they came, on one
// line ("breached:1 at_risk:2 ...").
func slaSlices(w *workWorld, who string) (int, map[string]int, string) {
	w.t.Helper()
	code, out := slaGet(w, who, projectController.Report, url.Values{"projects": {w.project}})
	var got struct {
		Data struct {
			Insights struct {
				Slices []struct {
					By   string `json:"by"`
					Key  string `json:"key"`
					Open int    `json:"open"`
					Rank int    `json:"rank"`
				} `json:"slices"`
			} `json:"insights"`
		} `json:"data"`
	}
	by := map[string]int{}
	var order []string
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			w.t.Fatalf("the report as %s: %v in %s", who, err, out)
		}
		for _, s := range got.Data.Insights.Slices {
			if s.By == "sla" {
				by[s.Key] = s.Open
				order = append(order, fmt.Sprintf("%s:%d", s.Key, s.Rank))
			}
		}
	}
	return code, by, strings.Join(order, " ")
}

// slaSliceBys is a report's slices on one line, by way: how many keys each
// way has, and which projects' tasks it holds.
func slaSliceBys(report string) string {
	var got struct {
		Data struct {
			Insights struct {
				Slices []struct {
					By  string `json:"by"`
					Key string `json:"key"`
				} `json:"slices"`
			} `json:"insights"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(report), &got); err != nil {
		return err.Error()
	}
	keys := map[string][]string{}
	for _, s := range got.Data.Insights.Slices {
		keys[s.By] = append(keys[s.By], s.Key)
	}
	return fmt.Sprint(len(got.Data.Insights.Slices), " ", keys)
}

func TestTheProjectListFiltersAndSortsBySLAForMembersOnly(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	integration.AcceptingOpenSearch(t)
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))
	slaGuest(w)
	// The report reads through each reader's own list of projects (Gus is on
	// his): Website is on Ben's, as joining it writes.
	w.onTheirLists("ben")
	switchTo := func(on string) {
		t.Helper()
		if _, err := policyBusiness.Admin.SetIn(context.Background(), policyBusiness.ProjectsSLA.Key, policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: w.project},
			json.RawMessage(on), policyBusiness.Changer{UserID: w.people["ana"], Email: "ana@acme.test"}); err != nil {
			t.Fatal(err)
		}
	}
	switchTo("true")
	// due moves a task's running timer: due from now, at risk from risk.
	due := func(task string, due, risk time.Duration) {
		t.Helper()
		if tm, found := slaTimerOf(w, task); !found || tm.state != "running" {
			t.Fatalf("%s's timer: %+v (found %v), want running; events %s", task, tm, found, slaEvents(w, task))
		}
		w.exec(`UPDATE task_slas SET due_at = $2, risk_at = $3 WHERE task_uuid = $1`, task, time.Now().Add(due), time.Now().Add(risk))
	}
	calm := slaMake(w, "Answer the calm ticket", "urgent")
	late := slaMake(w, "Answer the late ticket", "urgent")
	due(late, -time.Hour, -7*time.Hour)
	risky := slaMake(w, "Answer the risky ticket", "high")
	due(risky, 2*time.Hour, -time.Hour)
	met := slaMake(w, "Answer the met ticket", "urgent")
	slaDone(w, met)
	// Breached too, but kept from Ben: it stays hidden, and its SLA state with it.
	hidden := slaMake(w, "Answer the board's ticket", "urgent")
	due(hidden, -2*time.Hour, -8*time.Hour)
	if code, out := slaCall(w, "ana", http.MethodPut, taskRestrictionController.SetRestriction, map[string]string{"task_ref": hidden},
		map[string]any{"restricted": true, "people": []string{}}); code != http.StatusOK {
		t.Fatalf("restrict: %d %s", code, out)
	}
	integration.Settle(t)

	// Ben, a member: the filter keeps only those states.
	code, ids, out := slaList(w, "ben", `[{"id":"task_sla","value":["breached","at_risk"]}]`, "")
	if slices.Sort(ids); code != http.StatusOK || len(ids) != 2 || !slices.Contains(ids, late) || !slices.Contains(ids, risky) {
		t.Errorf("ben's breached or at risk: %d %v, want %s and %s; %s", code, ids, late, risky, out)
	}
	code, none, out := slaList(w, "ben", `[{"id":"task_sla","value":["none"]}]`, "")
	if code != http.StatusOK || !slices.Contains(none, w.task) || slices.ContainsFunc(none, func(id string) bool { return slices.Contains([]string{calm, late, risky, met, hidden}, id) }) {
		t.Errorf("ben's tasks with no SLA: %d %v; %s", code, none, out)
	}
	// The sort: breaches soonest, the hidden one nowhere.
	code, ids, out = slaList(w, "ben", "", `[{"id":"task_sla","desc":false}]`)
	if code != http.StatusOK || len(ids) < 4 || ids[0] != late || ids[1] != risky || ids[2] != calm || slices.Contains(ids, hidden) {
		t.Errorf("ben's list by SLA: %d %v, want %s, %s, %s first and %s nowhere; %s", code, ids, late, risky, calm, hidden, out)
	}
	// Insights by SLA: Ben's open work, without the hidden task, in urgency
	// order with ranks (no SLA last, 6, whatever states lie between).
	if code, by, order := slaSlices(w, "ben"); code != http.StatusOK || by["breached"] != 1 || by["at_risk"] != 1 || by["on_track"] != 1 || by[""] < 1 ||
		!strings.HasPrefix(order, "breached:1 at_risk:2 on_track:3 ") || !strings.HasSuffix(order, " :6") {
		benCtx := context.WithValue(context.Background(), helpers.UserInfoContextKey, w.account("ben"))
		reads, err := slaBusiness.ReadsIn(benCtx, []uuid.UUID{uuid.MustParse(w.project)})
		_, report := slaGet(w, "ben", projectController.Report, url.Values{"projects": {w.project}})
		t.Errorf("ben's Insights by SLA: %d %v in order [%s] (ben reads SLA state in %v, %v; the report's slices: %s)", code, by, order, reads, err, slaSliceBys(report))
	}
	// Ana, an admin who reads the restricted task, sees its breach.
	if code, ids, out = slaList(w, "ana", `[{"id":"task_sla","value":["breached"]}]`, ""); code != http.StatusOK || len(ids) != 2 || !slices.Contains(ids, hidden) {
		t.Errorf("ana's breached: %d %v, want %s and %s; %s", code, ids, late, hidden, out)
	}

	// Gus, a guest with a member's place: refused the filter and the sort
	// wherever he asks (the list, its CSV, the board, a board column, the
	// report); his plain list as it was; Insights with no SLA slices.
	if code, _, out = slaList(w, "gus", `[{"id":"task_sla","value":["breached"]}]`, ""); code != http.StatusForbidden {
		t.Errorf("a guest's SLA filter: %d %s, want 403", code, out)
	}
	if code, _, out = slaList(w, "gus", "", `[{"id":"task_sla","desc":false}]`); code != http.StatusForbidden {
		t.Errorf("a guest's SLA sort: %d %s, want 403", code, out)
	}
	if code, out = slaGet(w, "gus", projectController.Report, url.Values{"projects": {w.project}, "filters": {`[{"id":"task_sla","value":["breached"]}]`}}); code != http.StatusForbidden {
		t.Errorf("a guest's report filtered by SLA: %d %s, want 403", code, out)
	}
	byBreach := url.Values{"filters": {`[{"id":"task_sla","value":["breached"]}]`}}
	for _, ask := range []struct {
		name string
		h    http.HandlerFunc
		q    url.Values
	}{
		{"CSV filtered", projectController.GetProjectTaskList, url.Values{"format": {"csv"}, "tz": {"UTC"}, "filters": byBreach["filters"]}},
		{"CSV sorted", projectController.GetProjectTaskList, url.Values{"format": {"csv"}, "tz": {"UTC"}, "sorting": {`[{"id":"task_sla","desc":false}]`}}},
		{"board filtered", projectController.GetProjectTaskListForKanban, byBreach},
		{"board column filtered", projectController.GetProjectBoardColumn, url.Values{"status": {"todo"}, "filters": byBreach["filters"]}},
	} {
		if code, out = slaGet(w, "gus", ask.h, ask.q); code != http.StatusForbidden {
			t.Errorf("a guest's %s by SLA: %d %.200s, want 403", ask.name, code, out)
		}
	}
	if code, _, out = slaList(w, "gus", "", ""); code != http.StatusOK {
		t.Errorf("a guest's plain list: %d %s, want 200", code, out)
	}
	if code, by, _ := slaSlices(w, "gus"); code != http.StatusOK || len(by) != 0 {
		t.Errorf("a guest's Insights: %d, SLA slices %v, want none", code, by)
	}

	// SLAs off: the filter matches nothing, the sort is the list's own
	// (newest first), Insights has no SLA slices.
	switchTo("false")
	if code, ids, out = slaList(w, "ben", `[{"id":"task_sla","value":["breached","none"]}]`, ""); code != http.StatusOK || len(ids) != 0 {
		t.Errorf("SLAs off, ben's filter: %d %v, want nothing; %s", code, ids, out)
	}
	_, own, _ := slaList(w, "ben", "", "")
	if code, ids, out = slaList(w, "ben", "", `[{"id":"task_sla","desc":false}]`); code != http.StatusOK || !slices.Equal(ids, own) {
		t.Errorf("SLAs off, ben's sort: %d %v, want the list's own %v; %s", code, ids, own, out)
	}
	if code, by, _ := slaSlices(w, "ben"); code != http.StatusOK || len(by) != 0 {
		t.Errorf("SLAs off, ben's Insights: %d, SLA slices %v, want none", code, by)
	}
}
