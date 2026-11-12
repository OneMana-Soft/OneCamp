package business

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	taskViewModel "github.com/akashc777/OneCamp/models/postgres/TaskView"
	"github.com/google/uuid"
)

// A widget over a saved view (asana-08's "over saved queries") keeps the
// view's id and none of the view's query: its projects, filters and search
// are read from the view each time the widget is worked out.
func TestAWidgetOverASavedViewKeepsTheViewNotItsQuery(t *testing.T) {
	p, view := uuid.NewString(), uuid.NewString()
	project := Dashboard{Scope: ScopeProject, ProjectID: p}
	ws := Dashboard{Scope: ScopeWorkspace}
	copied := Query{View: " " + view + " ", Projects: []string{uuid.NewString()}, Search: "launch", Weeks: 8, Measure: "open",
		Filters: []taskDomain.TaskListParam{{ID: "task_priority", Value: []any{"high"}}}}
	at := func(d Dashboard, q Query) (Widget, error) {
		return CheckWidget(d, Widget{Kind: KindMetric, Query: q, W: 6, H: 3})
	}

	got, err := at(ws, copied)
	if err != nil {
		t.Fatalf("a widget over a view refused: %v", err)
	}
	want := Query{View: view, Weeks: 8, Measure: "open"}
	if !reflect.DeepEqual(got.Query, want) {
		t.Fatalf("the workspace's widget over a view keeps %+v, want %+v", got.Query, want)
	}
	copied.Projects = nil
	if got, err = at(project, copied); err != nil || !reflect.DeepEqual(got.Query, Query{View: view, Projects: []string{p}, Weeks: 8, Measure: "open"}) {
		t.Fatalf("a project's widget over a view keeps %+v (%v)", got.Query, err)
	}
	if _, err := at(ws, Query{View: "sprint", Measure: "open"}); err == nil {
		t.Fatal("a view that isn't one was kept")
	} else if _, is := err.(*Refusal); !is {
		t.Fatalf("%v is no Refusal", err)
	}
	// A goal widget reads its goal alone.
	goal := uuid.NewString()
	if got, err := CheckWidget(ws, Widget{Kind: KindGoal, Query: Query{Goal: goal, View: view}, W: 3, H: 2}); err != nil || got.Query.View != "" {
		t.Fatalf("a goal widget kept a view: %+v (%v)", got.Query, err)
	}
}

func ownView(scope string, filters string) viewSource {
	return viewSource{decided: true, decision: accessBusiness.Decision{Level: accessBusiness.LevelFull, Was: accessBusiness.LevelFull, Owner: true},
		view: &taskViewModel.TaskView{Name: "Sprint bugs", Scope: scope, State: json.RawMessage(`{"filters":` + filters + `,"sort":[]}`)}}
}

// A view is read as the reader: its owner reads the view's query as its list
// sends it; anyone else, and anyone after it is deleted, gets the widget's
// own word for why it can't show it, never the view's work.
func TestAWidgetReadsItsViewAsTheReader(t *testing.T) {
	p := uuid.NewString()
	high := `[{"id":"task_priority","value":["high"]},{"id":"task_status","value":["todo","in_progress"]}]`
	params := []taskDomain.TaskListParam{{ID: "task_priority", Value: []any{"high"}}, {ID: "task_status", Value: []any{"todo", "in_progress"}}}
	q := Query{View: uuid.NewString(), Weeks: 4, Measure: "cycle"}

	// A project's view: that project, with its filters.
	got, msg, name := fromView(q, ownView("project:"+p, high), "0x7")
	if msg != "" || name != "Sprint bugs" || !reflect.DeepEqual(got, Query{View: q.View, Projects: []string{p}, Filters: params, Weeks: 4, Measure: "cycle"}) {
		t.Fatalf("a project's view reads %+v %q %q", got, msg, name)
	}
	// My Tasks' view: the reader's tasks (My Tasks lists those assigned to
	// them) in every project they read, with its filters.
	got, msg, _ = fromView(q, ownView("mine", high), "0x7")
	mine := append(append([]taskDomain.TaskListParam{}, params...), taskDomain.TaskListParam{ID: "task_assignee_name", Value: []any{"0x7"}})
	if msg != "" || len(got.Projects) != 0 || !reflect.DeepEqual(got.Filters, mine) {
		t.Fatalf("My Tasks' view reads %+v %q", got, msg)
	}
	// On a project's dashboard, a view of that project reads; any other
	// can't be read there.
	q.Projects = []string{p}
	if got, msg, _ = fromView(q, ownView("project:"+p, `[]`), "0x7"); msg != "" || !reflect.DeepEqual(got.Projects, []string{p}) || len(got.Filters) != 0 {
		t.Fatalf("a project's dashboard over its own view reads %+v %q", got, msg)
	}
	for _, scope := range []string{"project:" + uuid.NewString(), "mine"} {
		if got, msg, _ = fromView(q, ownView(scope, high), "0x7"); msg != viewUnreadable || len(got.Filters) != 0 {
			t.Fatalf("a project's dashboard over a %s view reads %+v %q", scope, got, msg)
		}
	}
	q.Projects = nil

	unreadable := []struct {
		what string
		src  viewSource
		msg  string
	}{
		{"someone else's", viewSource{decided: true}, viewUnreadable},
		{"deleted by its owner", viewSource{decided: true, decision: accessBusiness.Decision{Deleted: true, Owner: true, Was: accessBusiness.LevelFull}}, viewGone},
		{"not there", viewSource{}, viewGone},
		{"readable but no longer live", viewSource{decided: true, decision: accessBusiness.Decision{Level: accessBusiness.LevelFull, Was: accessBusiness.LevelFull}}, viewGone},
		{"a state that can't be read", ownView("project:"+p, `"nope"`), viewBroken},
		{"a scope that isn't a list", ownView("project:nope", high), viewBroken},
	}
	for _, c := range unreadable {
		got, msg, name := fromView(q, c.src, "0x7")
		if msg != c.msg || name != "" || len(got.Filters) != 0 || len(got.Projects) != 0 {
			t.Errorf("a view %s reads %+v %q %q, want only %q", c.what, got, msg, name, c.msg)
		}
	}
}

// The views are decided in one batch through the one access rule, as the
// reader, and only those it lets them read are read.
func TestTheViewsAreReadAsTheReaderInOneBatch(t *testing.T) {
	mineID, othersID := uuid.New(), uuid.New()
	var asked []accessBusiness.Resource
	var who accessBusiness.Subject
	var read []uuid.UUID
	defer swapViewSeams(
		func(_ context.Context, s accessBusiness.Subject, res []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error) {
			who, asked = s, res
			return map[accessBusiness.Resource]accessBusiness.Decision{
				{Kind: workAccess.KindView, UUID: mineID.String()}:   {Level: accessBusiness.LevelFull, Was: accessBusiness.LevelFull},
				{Kind: workAccess.KindView, UUID: othersID.String()}: {},
			}, nil
		},
		func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]taskViewModel.TaskView, error) {
			read = ids
			return map[uuid.UUID]taskViewModel.TaskView{mineID: {Id: mineID, Scope: "mine", State: json.RawMessage(`{"filters":[]}`)}}, nil
		})()

	reader := Reader{UUID: uuid.New(), DgraphUID: "0x7"}
	widgets := []Widget{{Kind: KindMetric, Query: Query{View: mineID.String()}}, {Kind: KindFlow, Query: Query{View: othersID.String()}},
		{Kind: KindFlow, Query: Query{View: mineID.String()}}, {Kind: KindFlow}, {Kind: KindGoal, Query: Query{Goal: uuid.NewString()}}}
	got, err := readViews(context.Background(), reader, widgets)
	if err != nil {
		t.Fatal(err)
	}
	if who.UserUUID != reader.UUID.String() || who.UserUID != "0x7" || len(asked) != 2 {
		t.Fatalf("asked as %+v about %v", who, asked)
	}
	if len(read) != 1 || read[0] != mineID {
		t.Fatalf("read %v, want only the reader's view", read)
	}
	if src := got[mineID.String()]; src.view == nil || !src.decided {
		t.Fatalf("the reader's view: %+v", src)
	}
	if src := got[othersID.String()]; src.view != nil || !src.decided || src.decision.Level != accessBusiness.LevelNone {
		t.Fatalf("someone else's view: %+v", src)
	}

	failed := errors.New("down")
	defer swapViewSeams(func(context.Context, accessBusiness.Subject, []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error) {
		return nil, failed
	}, nil)()
	if _, err := readViews(context.Background(), reader, widgets); !errors.Is(err, failed) {
		t.Fatalf("a failed decision reads %v", err)
	}
	if got, err := readViews(context.Background(), reader, []Widget{{Kind: KindFlow}}); err != nil || len(got) != 0 {
		t.Fatalf("no views asked about: %v %v", got, err)
	}
}

// The keeper adds a widget over one of their own views, one that fits the
// dashboard: on a project's, one of that project's views.
func TestAKeeperAddsAWidgetOverTheirOwnView(t *testing.T) {
	p := uuid.New()
	views := map[string]taskViewModel.TaskView{}
	decisions := map[string]accessBusiness.Decision{}
	add := func(scope string, d accessBusiness.Decision) string {
		id := uuid.New()
		views[id.String()] = taskViewModel.TaskView{Id: id, Scope: scope, State: json.RawMessage(`{"filters":[]}`)}
		decisions[id.String()] = d
		return id.String()
	}
	own := accessBusiness.Decision{Level: accessBusiness.LevelFull, Was: accessBusiness.LevelFull, Owner: true}
	projectView, mineView := add("project:"+p.String(), own), add("mine", own)
	otherProject := add("project:"+uuid.NewString(), own)
	others := add("mine", accessBusiness.Decision{})
	defer fakeViews(views, decisions)()

	keeper := uuid.New()
	project := Dashboard{Scope: ScopeProject, ProjectID: p.String()}
	ws := Dashboard{Scope: ScopeWorkspace}
	for _, c := range []struct {
		what string
		d    Dashboard
		view string
		ok   bool
	}{
		{"its project's view on a project's", project, projectView, true},
		{"My Tasks' view on a project's", project, mineView, false},
		{"another project's view on a project's", project, otherProject, false},
		{"a project's view on the workspace's", ws, projectView, true},
		{"My Tasks' view on the workspace's", ws, mineView, true},
		{"someone else's view", ws, others, false},
		{"a view that isn't there", ws, uuid.NewString(), false},
	} {
		err := checkView(context.Background(), c.d, c.view, keeper)
		if c.ok && err != nil {
			t.Errorf("%s refused: %v", c.what, err)
		}
		if !c.ok {
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Errorf("%s: %v, want a Refusal", c.what, err)
			} else if strings.Contains(refusal.Error(), c.view) {
				t.Errorf("%s: the refusal names the view", c.what)
			}
		}
	}
}

// A keeper changing a widget keeps the view it reads already, even another
// keeper's (a retitle or a move); a view it newly reads must be one of their
// own, as on adding it.
func TestAChangedWidgetKeepsItsViewButRepointsOnlyToTheKeepersOwn(t *testing.T) {
	views := map[string]taskViewModel.TaskView{}
	decisions := map[string]accessBusiness.Decision{}
	add := func(d accessBusiness.Decision) string {
		id := uuid.New()
		views[id.String()] = taskViewModel.TaskView{Id: id, Scope: "mine", State: json.RawMessage(`{"filters":[]}`)}
		decisions[id.String()] = d
		return id.String()
	}
	own := add(accessBusiness.Decision{Level: accessBusiness.LevelFull, Was: accessBusiness.LevelFull, Owner: true})
	theirs, theirOther := add(accessBusiness.Decision{}), add(accessBusiness.Decision{})
	defer fakeViews(views, decisions)()

	d := Dashboard{ID: uuid.NewString(), Scope: ScopeWorkspace}
	overTheirs, plain := uuid.NewString(), uuid.NewString()
	var asked string
	kept := []Widget{{ID: overTheirs, Kind: KindMetric, Query: Query{View: theirs}}, {ID: plain, Kind: KindFlow}}
	oldKept := keptWidgets
	keptWidgets = func(_ context.Context, of Dashboard) ([]Widget, error) { asked = of.ID; return kept, nil }
	defer func() { keptWidgets = oldKept }()

	keeper := uuid.New()
	for _, c := range []struct {
		what   string
		widget string
		view   string
		ok     bool
	}{
		{"retitling a widget over another keeper's view", overTheirs, theirs, true},
		{"repointing it to their own view", overTheirs, own, true},
		{"repointing it to another view of someone else's", overTheirs, theirOther, false},
		{"pointing another widget at a view one widget reads", plain, theirs, false},
		{"pointing another widget at their own view", plain, own, true},
		{"a widget not on the dashboard, at a view one widget reads", uuid.NewString(), theirs, false},
	} {
		err := checkChangedView(context.Background(), d, Widget{ID: c.widget, Query: Query{View: c.view}}, keeper)
		if c.ok && err != nil {
			t.Errorf("%s refused: %v", c.what, err)
		}
		if !c.ok {
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Errorf("%s: %v, want a Refusal", c.what, err)
			}
		}
	}
	if asked != d.ID {
		t.Fatalf("read the widgets of %q, want the dashboard's own %q", asked, d.ID)
	}
	if err := checkChangedView(context.Background(), d, Widget{ID: "not-an-id", Query: Query{View: theirs}}, keeper); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a widget id that isn't one reads %v, want ErrNotFound", err)
	}
	failed := errors.New("down")
	keptWidgets = func(context.Context, Dashboard) ([]Widget, error) { return nil, failed }
	if err := checkChangedView(context.Background(), d, Widget{ID: overTheirs, Query: Query{View: theirs}}, keeper); !errors.Is(err, failed) {
		t.Fatalf("a failed read of the widgets reads %v", err)
	}
}

// fakeViews answers the view seams from views and decisions, by id.
func fakeViews(views map[string]taskViewModel.TaskView, decisions map[string]accessBusiness.Decision) func() {
	return swapViewSeams(
		func(_ context.Context, _ accessBusiness.Subject, res []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error) {
			out := map[accessBusiness.Resource]accessBusiness.Decision{}
			for _, r := range res {
				if d, ok := decisions[r.UUID]; ok {
					out[r] = d
				}
			}
			return out, nil
		},
		func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]taskViewModel.TaskView, error) {
			out := map[uuid.UUID]taskViewModel.TaskView{}
			for _, id := range ids {
				if v, ok := views[id.String()]; ok {
					out[id] = v
				}
			}
			return out, nil
		})
}

func swapViewSeams(decide func(context.Context, accessBusiness.Subject, []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error),
	live func(context.Context, []uuid.UUID) (map[uuid.UUID]taskViewModel.TaskView, error)) func() {
	oldDecide, oldLive := decideViews, liveViews
	if decide != nil {
		decideViews = decide
	}
	if live != nil {
		liveViews = live
	}
	return func() { decideViews, liveViews = oldDecide, oldLive }
}
