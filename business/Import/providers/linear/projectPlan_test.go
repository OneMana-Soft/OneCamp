package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// A Linear project's plan comes as the import takes it: the lead's id, each
// date at its resolution (halfYear is a half; none is a day), the priority
// by the name its issues' priorities have, the status by its type's
// category with its own name and colour, and every label.
func TestLinearPlanComesAsTheImportTakesIt(t *testing.T) {
	var l linearPlan
	node := `{"lead":{"id":"usr-1"},"startDate":"2026-10-01","startDateResolution":"quarter",
		"targetDate":"2027-06-30","targetDateResolution":"halfYear","priority":2,
		"status":{"type":"paused","name":"On hold","color":"#f2994a"},
		"labels":{"nodes":[{"name":"Mobile","color":"#4ea7fc"},{"name":"Q3 bet","color":"#bec2c8"}]}}`
	if err := json.Unmarshal([]byte(node), &l); err != nil {
		t.Fatal(err)
	}
	p := l.source()
	if p == nil {
		t.Fatal("no plan")
	}
	if p.LeadID != "usr-1" || p.Priority != "high" {
		t.Errorf("lead %q, priority %q; want usr-1, high", p.LeadID, p.Priority)
	}
	if p.Start == nil || p.Start.Day.String() != "2026-10-01" || p.Start.Resolution != "quarter" {
		t.Errorf("start: got %+v", p.Start)
	}
	if p.Target == nil || p.Target.Day.String() != "2027-06-30" || p.Target.Resolution != "half" {
		t.Errorf("target: got %+v", p.Target)
	}
	want := importProvider.SourcePlanStatus{Category: "paused", Name: "On hold", Color: "#f2994a"}
	if p.Status != want {
		t.Errorf("status: got %+v, want %+v", p.Status, want)
	}
	if len(p.Labels) != 2 || p.Labels[0] != (importProvider.SourceLabel{Name: "Mobile", Color: "#4ea7fc"}) || p.Labels[1].Name != "Q3 bet" {
		t.Errorf("labels: got %+v", p.Labels)
	}
}

// Each of Linear's status types falls in its category; a date with no
// resolution is a day; no lead, no priority and no dates come as none.
func TestLinearPlanCategoriesAndEmptyParts(t *testing.T) {
	for typ, want := range map[string]string{
		"backlog": "planned", "planned": "planned", "started": "in_progress",
		"paused": "paused", "completed": "done", "canceled": "cancelled", "triage": "",
	} {
		var l linearPlan
		l.Status.Type = typ
		p := l.source()
		if p == nil {
			t.Fatalf("%s: no plan", typ)
		}
		if got := p.Status.Category; got != want {
			t.Errorf("%s: got %q, want %q", typ, got, want)
		}
	}
	var l linearPlan
	if err := json.Unmarshal([]byte(`{"lead":null,"startDate":"2026-12-07","startDateResolution":null,"targetDate":null,"priority":0,"status":{"type":"started","name":"In Progress"}}`), &l); err != nil {
		t.Fatal(err)
	}
	p := l.source()
	if p == nil {
		t.Fatal("no plan")
	}
	if p.LeadID != "" || p.Priority != "" || p.Target != nil || p.Start == nil || p.Start.Resolution != "day" {
		t.Errorf("got lead %q, priority %q, target %+v, start %+v", p.LeadID, p.Priority, p.Target, p.Start)
	}
}

// An issue enters its state when Linear says: completed or cancelled at
// their times, started at its start while it's started, and otherwise
// Linear gives no time.
func TestIssueStatusSince(t *testing.T) {
	at := func(d int) *time.Time { v := time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC); return &v }
	for _, c := range []struct {
		name string
		iss  linearIssue
		want *time.Time
	}{
		{"completed", linearIssue{StateType: "completed", StartedAt: at(2), CompletedAt: at(9)}, at(9)},
		{"cancelled", linearIssue{StateType: "canceled", CanceledAt: at(7)}, at(7)},
		{"started", linearIssue{StateType: "started", StartedAt: at(4)}, at(4)},
		{"back in the backlog", linearIssue{StateType: "backlog", StartedAt: at(4)}, nil},
		{"unstarted", linearIssue{StateType: "unstarted"}, nil},
	} {
		got := issueStatusSince(c.iss)
		if (got == nil) != (c.want == nil) || (got != nil && !got.Equal(*c.want)) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// CR-163 row 2279 (F1): a workspace read from Linear's API, through the
// snapshot the import builds, gives the project stage its project's plan and
// the task stages each issue's time in its state: a done issue's
// completedAt, a cancelled one's canceledAt, a started one's startedAt, none
// for one not started, and a done sub-issue's completedAt in both streams.
func TestAWorkspaceReadFromLinearCarriesThePlanAndTheTimes(t *testing.T) {
	const none = `{"nodes": [], "pageInfo": {"hasNextPage": false, "endCursor": ""}}`
	issue := func(id, state, times, parent string) string {
		p := "null"
		if parent != "" {
			p = `{"id": "` + parent + `"}`
		}
		return `{"id": "` + id + `", "identifier": "ENG-` + id + `", "title": "` + id + `", "createdAt": "2026-09-01T09:00:00Z",
			"updatedAt": "2026-10-20T09:00:00Z", ` + times + `, "state": {"id": "s-` + state + `", "name": "` + state + `", "type": "` + state + `"},
			"parent": ` + p + `, "team": {"id": "eng"}, "project": {"id": "proj"}}`
	}
	issues := strings.Join([]string{
		issue("done", "completed", `"startedAt": "2026-10-02T12:00:00Z", "completedAt": "2026-10-09T12:00:00Z"`, ""),
		issue("dropped", "canceled", `"canceledAt": "2026-10-07T12:00:00Z"`, ""),
		issue("going", "started", `"startedAt": "2026-10-04T12:00:00Z"`, ""),
		issue("later", "unstarted", `"startedAt": null`, ""),
		issue("sub", "completed", `"completedAt": "2026-10-08T15:00:00Z"`, "done"),
	}, ",")
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := readBody(r)
		switch {
		case strings.Contains(body, "team(id:"):
			_, _ = w.Write([]byte(`{"data": {"team": {"issues": {"nodes": [` + issues + `], "pageInfo": {"hasNextPage": false, "endCursor": ""}}}}}`))
		case strings.Contains(body, "projects("):
			_, _ = w.Write([]byte(`{"data": {"projects": {"nodes": [{"id": "proj", "name": "Launch", "description": "", "url": "https://linear.app/acme/project/launch",
				"status": {"type": "paused", "name": "On hold", "color": "#f2994a"}, "lead": {"id": "usr-1"},
				"startDate": "2026-10-01", "startDateResolution": "quarter", "targetDate": "2027-06-30", "targetDateResolution": "halfYear", "priority": 2,
				"labels": {"nodes": [{"name": "Mobile", "color": "#4ea7fc"}]}, "teams": {"nodes": [{"id": "eng"}]}, "members": {"nodes": [{"id": "usr-1"}]}}],
				"pageInfo": {"hasNextPage": false, "endCursor": ""}}}}`))
		case strings.Contains(body, "teams(first: 50"):
			_, _ = w.Write([]byte(`{"data": {"teams": {"nodes": [{"id": "eng", "key": "ENG", "name": "Eng"}], "pageInfo": {"hasNextPage": false, "endCursor": ""}}}}`))
		default: // users, labels, cycles and milestones: none
			_, _ = w.Write([]byte(`{"data": {"users": ` + none + `, "issueLabels": ` + none + `, "cycles": ` + none + `, "projectMilestones": ` + none + `}}`))
		}
	})
	ctx := context.Background()
	p, job := New(), &importModels.Job{Id: uuid.New()}
	if _, err := p.snapshotWith(ctx, job, "tok"); err != nil {
		t.Fatal(err)
	}

	on := func(s string) importProvider.Day {
		d, ok := importProvider.ParseDay(s)
		if !ok {
			t.Fatalf("bad day %q", s)
		}
		return d
	}
	want := &importProvider.SourcePlan{
		LeadID:   "usr-1",
		Start:    &importProvider.SourceWhen{Day: on("2026-10-01"), Resolution: "quarter"},
		Target:   &importProvider.SourceWhen{Day: on("2027-06-30"), Resolution: "half"},
		Priority: "high",
		Status:   importProvider.SourcePlanStatus{Category: "paused", Name: "On hold", Color: "#f2994a"},
		Labels:   []importProvider.SourceLabel{{Name: "Mobile", Color: "#4ea7fc"}},
	}
	projects, errs := p.IterProjects(ctx, job, nil)
	plans := map[string]*importProvider.SourcePlan{}
	for sp := range projects {
		plans[sp.SourceID] = sp.Plan
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if got, ok := plans["proj"]; !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("Launch's plan %+v, want %+v (projects %v)", got, want, plans)
	}

	at := func(s string) *time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return &v
	}
	since := map[string]*time.Time{}
	tasks, errs := p.IterTasksOfProject(ctx, job, nil, "proj")
	for st := range tasks {
		since[st.SourceID] = st.StatusSince
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	subs, errs := p.IterSubtasksOfTask(ctx, job, nil, "done")
	for st := range subs {
		since["sub of done: "+st.SourceID] = st.StatusSince
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	for id, w := range map[string]*time.Time{
		"done": at("2026-10-09T12:00:00Z"), "dropped": at("2026-10-07T12:00:00Z"), "going": at("2026-10-04T12:00:00Z"), "later": nil,
		"sub": at("2026-10-08T15:00:00Z"), "sub of done: sub": at("2026-10-08T15:00:00Z"),
	} {
		got, ok := since[id]
		if !ok || (got == nil) != (w == nil) || (got != nil && !got.Equal(*w)) {
			t.Errorf("%s entered its state at %v (streamed %v), want %v", id, got, ok, w)
		}
	}
	if len(since) != 6 {
		t.Errorf("streamed %d issues, want the project's five and done's one sub-issue: %v", len(since), since)
	}
}

// L2 (main 20:5x, CR-163 row 2279): a project's labels come whole, a page at
// a time from the project after the first page its projects query gives,
// following Linear's own cursor; a page Linear doesn't give leaves the rest
// out, and the plan says so for the import's report.
func TestEveryLabelOfAProjectComesPageByPage(t *testing.T) {
	label := func(i int) string { return fmt.Sprintf(`{"name": "label %03d", "color": "#4ea7fc"}`, i) }
	page := func(from, to int) string {
		var nodes []string
		for i := from; i < to; i++ {
			nodes = append(nodes, label(i))
		}
		return strings.Join(nodes, ",")
	}
	for _, c := range []struct {
		name   string
		broken bool // the third page is refused
		want   int
		unread string
	}{
		{"every page", false, 150, ""},
		{"a page refused", true, 120, `its labels after the first 120 (Linear didn't give them: linear graphql: `},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked []string
			withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				body, _ := readBody(r)
				switch {
				case strings.Contains(body, "project(id:"):
					asked = append(asked, body)
					switch {
					case strings.Contains(body, `"cursor":"c1"`):
						_, _ = w.Write([]byte(`{"data": {"project": {"labels": {"nodes": [` + page(20, 120) + `], "pageInfo": {"hasNextPage": true, "endCursor": "c2"}}}}}`))
					case strings.Contains(body, `"cursor":"c2"`) && c.broken:
						_, _ = w.Write([]byte(`{"errors": [{"message": "Query timed out"}]}`))
					case strings.Contains(body, `"cursor":"c2"`):
						_, _ = w.Write([]byte(`{"data": {"project": {"labels": {"nodes": [` + page(120, 150) + `], "pageInfo": {"hasNextPage": false, "endCursor": ""}}}}}`))
					default:
						t.Errorf("a labels page not after the last: %s", body)
					}
				case strings.Contains(body, "projects("):
					_, _ = w.Write([]byte(`{"data": {"projects": {"nodes": [{"id": "proj", "name": "Launch",
						"labels": {"nodes": [` + page(0, 20) + `], "pageInfo": {"hasNextPage": true, "endCursor": "c1"}}}],
						"pageInfo": {"hasNextPage": false, "endCursor": ""}}}}`))
				default:
					t.Errorf("not a projects or labels query: %s", body)
				}
			})
			got, err := New().fetchProjects(context.Background(), "tok")
			if err != nil || len(got) != 1 || got[0].Plan == nil {
				t.Fatalf("projects %+v, %v", got, err)
			}
			plan := got[0].Plan
			if len(plan.Labels) != c.want {
				t.Fatalf("%d labels, want %d", len(plan.Labels), c.want)
			}
			for i, l := range plan.Labels {
				if want := fmt.Sprintf("label %03d", i); l.Name != want || l.Color != "#4ea7fc" {
					t.Fatalf("label %d is %+v, want %s in its colour, in Linear's order", i, l, want)
				}
			}
			if c.unread == "" && len(plan.Unread) != 0 || c.unread != "" && (len(plan.Unread) != 1 || !strings.HasPrefix(plan.Unread[0], c.unread)) {
				t.Fatalf("unread %q, want %q", plan.Unread, c.unread)
			}
			for _, q := range asked {
				if !strings.Contains(q, `"id":"proj"`) {
					t.Errorf("a labels page not of the project: %s", q)
				}
			}
		})
	}
}
