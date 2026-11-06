package business

import (
	"encoding/json"
	"strings"
	"testing"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// The permission model's rules, case by case (the package comment's 1-3).
func TestWhoMayReadAndKeepADashboard(t *testing.T) {
	owner, other := uuid.New(), uuid.New()
	stand := func(r workAccess.Role, archived bool) workAccess.Standing {
		return workAccess.Standing{Role: r, Archived: archived}
	}
	none := workAccess.Standing{}
	steward := workAccess.Standing{Steward: true}
	mine := Dashboard{Scope: ScopeMine, MineOf: owner}
	project := Dashboard{Scope: ScopeProject, ProjectID: uuid.NewString()}
	sharedProject := project
	sharedProject.Shared = true
	workspace := Dashboard{Scope: ScopeWorkspace}
	sharedWorkspace := Dashboard{Scope: ScopeWorkspace, Shared: true}
	for _, c := range []struct {
		name       string
		d          Dashboard
		who        uuid.UUID
		s          workAccess.Standing
		admin      bool
		read, keep bool
	}{
		{"mine: its owner", mine, owner, none, false, true, true},
		{"mine: anyone else, a workspace admin too", mine, other, none, true, false, false},
		{"project, unshared: its admin", project, other, stand(workAccess.RoleAdmin, false), false, true, true},
		{"project, unshared: its member", project, other, stand(workAccess.RoleMember, false), false, false, false},
		{"project, shared: its member", sharedProject, other, stand(workAccess.RoleMember, false), false, true, false},
		{"project, shared: its commenter", sharedProject, other, stand(workAccess.RoleCommenter, false), false, true, false},
		{"project, shared: its viewer", sharedProject, other, stand(workAccess.RoleViewer, false), false, true, false},
		{"project, shared: outside it, a workspace admin too", sharedProject, other, none, true, false, false},
		{"project, shared: the team's admin outside it", sharedProject, other, steward, false, false, false},
		{"project, archived: its admin reads, never keeps", sharedProject, other, stand(workAccess.RoleAdmin, true), false, true, false},
		{"workspace, unshared: an admin", workspace, other, none, true, true, true},
		{"workspace, unshared: a member", workspace, other, none, false, false, false},
		{"workspace, shared: a member", sharedWorkspace, other, none, false, true, false},
	} {
		if got := MayRead(c.d, c.who, c.s, c.admin); got != c.read {
			t.Errorf("%s reads: %v, want %v", c.name, got, c.read)
		}
		if got := MayKeep(c.d, c.who, c.s, c.admin); got != c.keep {
			t.Errorf("%s keeps: %v, want %v", c.name, got, c.keep)
		}
	}
}

func TestAWidgetIsCheckedBeforeItIsKept(t *testing.T) {
	p := uuid.NewString()
	project := Dashboard{Scope: ScopeProject, ProjectID: p}
	ws := Dashboard{Scope: ScopeWorkspace}
	ok := func(d Dashboard, w Widget) Widget {
		t.Helper()
		got, err := CheckWidget(d, w)
		if err != nil {
			t.Fatalf("%+v refused: %v", w, err)
		}
		return got
	}
	refused := func(what string, d Dashboard, w Widget) {
		t.Helper()
		if _, err := CheckWidget(d, w); err == nil {
			t.Errorf("%s: kept", what)
		} else if _, is := err.(*Refusal); !is {
			t.Errorf("%s: %v is no Refusal", what, err)
		}
	}
	at := func(w Widget) Widget { w.X, w.Y, w.W, w.H = 0, 0, 6, 3; return w }

	// A project's widget is about the project alone, named or not.
	if got := ok(project, at(Widget{Kind: KindThroughput, Title: "  Done  "})); len(got.Query.Projects) != 1 || got.Query.Projects[0] != p || got.Title != "Done" {
		t.Fatalf("a project's widget: %+v", got)
	}
	refused("a project's widget on another", project, at(Widget{Kind: KindThroughput, Query: Query{Projects: []string{uuid.NewString()}}}))
	refused("a project's widget on two", project, at(Widget{Kind: KindThroughput, Query: Query{Projects: []string{p, uuid.NewString()}}}))
	// The workspace's names any, each once, at most MaxProjects.
	a := uuid.NewString()
	if got := ok(ws, at(Widget{Kind: KindFlow, Query: Query{Projects: []string{a, " " + a + " "}}})); len(got.Query.Projects) != 1 {
		t.Fatalf("a project named twice: %+v", got.Query)
	}
	many := make([]string, MaxProjects+1)
	for i := range many {
		many[i] = uuid.NewString()
	}
	refused("too many projects", ws, at(Widget{Kind: KindFlow, Query: Query{Projects: many}}))
	refused("a project that isn't one", ws, at(Widget{Kind: KindFlow, Query: Query{Projects: []string{"nope"}}}))
	refused("a kind there isn't", ws, at(Widget{Kind: "pie"}))
	refused("a metric without a measure", ws, at(Widget{Kind: KindMetric}))
	refused("a breakdown by status", ws, at(Widget{Kind: KindBreakdown, Query: Query{By: "status"}}))
	refused("a goal widget without a goal", ws, at(Widget{Kind: KindGoal}))
	refused("53 weeks", ws, at(Widget{Kind: KindThroughput, Query: Query{Weeks: 53}}))
	refused("a long search", ws, at(Widget{Kind: KindThroughput, Query: Query{Search: strings.Repeat("a", MaxSearch+1)}}))
	refused("too many filters", ws, at(Widget{Kind: KindThroughput, Query: Query{Filters: make([]taskDomain.TaskListParam, MaxFilters+1)}}))
	refused("a long title", ws, at(Widget{Kind: KindThroughput, Title: strings.Repeat("t", MaxName+1)}))
	// What a kind doesn't read is dropped; a goal widget reads its goal alone.
	if got := ok(ws, at(Widget{Kind: KindMetric, Query: Query{Measure: "cycle", By: "person", Goal: a}})); got.Query.By != "" || got.Query.Goal != "" {
		t.Fatalf("a metric keeps what it doesn't read: %+v", got.Query)
	}
	if got := ok(ws, at(Widget{Kind: KindGoal, Query: Query{Goal: a, Projects: []string{a}, Search: "x"}})); got.Query.Goal != a || len(got.Query.Projects) != 0 || got.Query.Search != "" {
		t.Fatalf("a goal widget: %+v", got.Query)
	}
	// The grid.
	for _, c := range [][4]int{{-1, 0, 3, 2}, {10, 0, 3, 2}, {0, 0, 0, 2}, {0, 0, 13, 2}, {0, -1, 3, 2}, {0, MaxRow + 1, 3, 2}, {0, 0, 3, 0}, {0, 0, 3, MaxHeight + 1}} {
		w := Widget{Kind: KindFlow, X: c[0], Y: c[1], W: c[2], H: c[3]}
		refused("off the grid", ws, w)
	}
	ok(ws, Widget{Kind: KindFlow, X: 6, Y: MaxRow, W: 6, H: MaxHeight})
}

func TestArrangingPlacesEachWidgetOnceOnTheGrid(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	if err := CheckArrange([]Place{{ID: a, W: 6, H: 2}, {ID: b, X: 6, W: 6, H: 2}}); err != nil {
		t.Fatal(err)
	}
	for name, places := range map[string][]Place{
		"twice":       {{ID: a, W: 6, H: 2}, {ID: a, X: 6, W: 6, H: 2}},
		"not an id":   {{ID: "x", W: 6, H: 2}},
		"off grid":    {{ID: a, X: 7, W: 6, H: 2}},
		"too many":    make([]Place, MaxWidgets+1),
		"no height":   {{ID: b, W: 6}},
		"left of one": {{ID: b, X: -1, W: 6, H: 1}},
	} {
		if err := CheckArrange(places); err == nil {
			t.Errorf("%s: arranged", name)
		}
	}
}

func TestNamesAndSharing(t *testing.T) {
	if got, err := CheckName("  Launch  "); err != nil || got != "Launch" {
		t.Fatalf("a name: %q %v", got, err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("n", MaxName+1)} {
		if _, err := CheckName(bad); err == nil {
			t.Errorf("%q named", bad)
		}
	}
	if err := CheckShared(Dashboard{Scope: ScopeMine}, true); err == nil {
		t.Error("a person's own was shared")
	}
	if err := CheckShared(Dashboard{Scope: ScopeProject}, true); err != nil {
		t.Error(err)
	}
}

// Widgets asking the same question share one report, whatever order their
// projects come in; any difference asks again.
func TestWidgetsAskingTheSameShareOneReport(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	base := Query{Projects: []string{a, b}, Weeks: 4, Measure: "open"}
	same := Query{Projects: []string{b, a}, Weeks: 4, Measure: "cycle", By: "person"}
	if askKey(base) != askKey(same) {
		t.Fatal("the same question asked twice")
	}
	for _, q := range []Query{
		{Projects: []string{a}, Weeks: 4},
		{Projects: []string{a, b}, Weeks: 8},
		{Projects: []string{a, b}, Weeks: 4, Search: "x"},
		{Projects: []string{a, b}, Weeks: 4, Filters: []taskDomain.TaskListParam{{ID: "priority", Value: []string{"high"}}}},
	} {
		if askKey(q) == askKey(base) {
			t.Errorf("%+v shares base's report", q)
		}
	}
}

func TestEachKindShowsItsPartOfTheReport(t *testing.T) {
	onTime := 75
	rep := &projectBusiness.Report{Weeks: []string{"w1", "w2"}, Done: []int{1, 2}, Added: []int{3, 4}, Open: 5, Overdue: 2,
		DueThisWeek: 1, DoneTotal: 3, OnTime: &onTime,
		Projects:   []projectBusiness.ReportProjectRow{{UUID: "p"}},
		People:     []projectBusiness.ReportPersonRow{{UUID: "u"}},
		Priorities: []projectBusiness.ReportPriorityRow{{Priority: "high"}},
		Insights:   projectBusiness.Insights{Cycle: helpers.Spread{N: 3, P50: 2.5}}}
	asJSON := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	for _, c := range []struct {
		w    Widget
		want string
	}{
		{Widget{Kind: KindMetric, Query: Query{Measure: "open"}}, `{"measure":"open","value":5}`},
		{Widget{Kind: KindMetric, Query: Query{Measure: "overdue"}}, `{"measure":"overdue","value":2}`},
		{Widget{Kind: KindMetric, Query: Query{Measure: "due_this_week"}}, `{"measure":"due_this_week","value":1}`},
		{Widget{Kind: KindMetric, Query: Query{Measure: "done_total"}}, `{"measure":"done_total","value":3}`},
		{Widget{Kind: KindMetric, Query: Query{Measure: "on_time"}}, `{"measure":"on_time","value":75}`},
		{Widget{Kind: KindMetric, Query: Query{Measure: "cycle"}}, `{"measure":"cycle","value":2.5,"spread":{"n":3,"p25":0,"p50":2.5,"p85":0,"p95":0}}`},
		{Widget{Kind: KindMetric, Query: Query{Measure: "lead"}}, `{"measure":"lead","value":null,"spread":{"n":0,"p25":0,"p50":0,"p85":0,"p95":0}}`},
		{Widget{Kind: KindThroughput}, `{"added":[3,4],"done":[1,2],"weeks":["w1","w2"]}`},
		{Widget{Kind: KindBreakdown, Query: Query{By: "person"}}, `{"by":"person","rows":[{"user_uuid":"u","to_do":0,"in_progress":0,"in_review":0,"overdue":0,"done":0}]}`},
		{Widget{Kind: KindBreakdown, Query: Query{By: "priority"}}, `{"by":"priority","rows":[{"priority":"high","open":0,"overdue":0}]}`},
	} {
		if got := asJSON(shape(c.w, rep)); got != c.want {
			t.Errorf("%s %+v: %s, want %s", c.w.Kind, c.w.Query, got, c.want)
		}
	}
	if got := asJSON(shape(Widget{Kind: KindBreakdown, Query: Query{By: "project"}}, rep)); !strings.Contains(got, `"project_uuid":"p"`) {
		t.Errorf("by project: %s", got)
	}
	// Nothing on time yet reads as nothing, not 0%.
	rep.OnTime = nil
	if got := asJSON(shape(Widget{Kind: KindMetric, Query: Query{Measure: "on_time"}}, rep)); got != `{"measure":"on_time","value":null}` {
		t.Errorf("on time with nothing due: %s", got)
	}
}

// A widget's query is its keeper's: one who keeps the dashboard reads it
// whole; a reader, only what draws the widget, never its projects, filters,
// search or goal (case 5 of the permission model).
func TestAReaderSeesAWidgetsShapeNeverItsQuery(t *testing.T) {
	full := Query{Projects: []string{"p-website", "p-launch"}, Filters: []taskDomain.TaskListParam{{ID: "label", Value: "secret-launch"}},
		Search: "acquisition", Weeks: 8, Measure: "open", By: "project", Goal: "g-revenue"}
	widgets := []Widget{{ID: "w1", Kind: KindMetric, Title: "Launch", Query: full, X: 3, Y: 2, W: 6, H: 3}}
	asJSON := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	whole := asJSON(widgets)

	if got := asJSON(Shown(widgets, true)); got != whole {
		t.Fatalf("one who keeps it reads %s, want the whole %s", got, whole)
	}
	read := Shown(widgets, false)
	want := `[{"id":"w1","kind":"metric","title":"Launch","query":{"weeks":8,"measure":"open","by":"project"},"x":3,"y":2,"w":6,"h":3}]`
	if got := asJSON(read); got != want {
		t.Fatalf("a reader reads %s, want %s", got, want)
	}
	for _, kept := range []string{"p-website", "p-launch", "secret-launch", "acquisition", "g-revenue"} {
		if strings.Contains(asJSON(read), kept) {
			t.Fatalf("a reader's widgets name %q", kept)
		}
	}
	if asJSON(widgets) != whole {
		t.Fatalf("Shown changed the widgets it was given: %s", asJSON(widgets))
	}
	if got := Shown(nil, false); len(got) != 0 {
		t.Fatalf("no widgets read as %v", got)
	}
}
