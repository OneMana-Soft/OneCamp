package business

// A widget over a saved view (B9-b4, asana-08's "over saved queries"): its
// Query names the view, and keeps none of the view's query. Each time the
// widget is worked out, the view is read AS THE READER, through the one
// access rule for a saved view (business/WorkAccess: its owner's alone; a
// deleted one is none), and its filters go to the report the way its list
// sends them (projectBusiness.ReportFor, the list's own builder). So the
// widget follows the view as it changes, says so when the view has gone, and
// gives anyone the view isn't for its own "can't read" word, never the view's
// work.

import (
	"context"
	"encoding/json"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	taskViewModel "github.com/akashc777/OneCamp/models/postgres/TaskView"
	"github.com/google/uuid"
)

// What a widget over a view says when it can't show it.
const (
	viewGone       = "That saved view isn't there any more."
	viewUnreadable = "This widget reads a saved view only its owner can see."
	viewBroken     = "This widget's saved view can't be read. Choose it again."
)

// The saved view's scopes (models/postgres/TaskView): a project's task list,
// or My Tasks.
const (
	viewScopeMine    = "mine"
	viewScopeProject = "project:"
)

// Seams for tests: the one access rule, asked in one batch, and the read of
// the live views it lets through.
var (
	decideViews = accessBusiness.ResolveAll
	liveViews   = taskViewModel.LiveByID
)

// viewSource is a widget's saved view as one reader stands to it: whether
// the access rule decided it at all (a view that isn't there isn't), its
// decision, and the view itself when the reader may read it and it is live.
type viewSource struct {
	decided  bool
	decision accessBusiness.Decision
	view     *taskViewModel.TaskView
}

// readViews is every view widgets read, by its id, as r stands to it: one
// batch through the access rule, then one read of the views r may read.
func readViews(ctx context.Context, r Reader, widgets []Widget) (map[string]viewSource, error) {
	out := map[string]viewSource{}
	var res []accessBusiness.Resource
	for _, w := range widgets {
		if id := w.Query.View; id != "" && w.Kind != KindGoal {
			if _, seen := out[id]; !seen {
				out[id] = viewSource{}
				res = append(res, accessBusiness.Resource{Kind: workAccess.KindView, UUID: id})
			}
		}
	}
	if len(res) == 0 {
		return out, nil
	}
	who := accessBusiness.Subject{UserUID: r.DgraphUID, UserUUID: r.UUID.String(), Admin: r.IsAdmin}
	decisions, err := decideViews(ctx, who, res)
	if err != nil {
		return nil, err
	}
	var mayRead []uuid.UUID
	for _, one := range res {
		d, ok := decisions[one]
		out[one.UUID] = viewSource{decided: ok, decision: d}
		if id, err := uuid.Parse(one.UUID); err == nil && ok && d.Level >= accessBusiness.LevelView {
			mayRead = append(mayRead, id)
		}
	}
	views, err := liveViews(ctx, mayRead)
	if err != nil {
		return nil, err
	}
	for id, v := range views {
		src := out[id.String()]
		src.view = &v
		out[id.String()] = src
	}
	return out, nil
}

// fromView is q with its view's projects and filters as the reader (graph
// uid readerUID) reads the view src, and the view's name; or, when they
// can't, the widget's word for why, with nothing of the view. A project's
// view reads that project; My Tasks' view reads the reader's tasks (those
// assigned to them, as My Tasks lists) in every project they read. On a
// project's dashboard (q.Projects is its one project) only a view of that
// project reads. Pure.
func fromView(q Query, src viewSource, readerUID string) (Query, string, string) {
	bare := Query{View: q.View, Weeks: q.Weeks, Measure: q.Measure, By: q.By}
	switch {
	case !src.decided || (src.decision.Level < accessBusiness.LevelView && src.decision.Was >= accessBusiness.LevelView):
		return bare, viewGone, ""
	case src.decision.Level < accessBusiness.LevelView:
		return bare, viewUnreadable, ""
	case src.view == nil:
		// Decided readable, then gone before it was read.
		return bare, viewGone, ""
	}
	var state struct {
		Filters []taskDomain.TaskListParam `json:"filters"`
	}
	if err := json.Unmarshal(src.view.State, &state); err != nil {
		return bare, viewBroken, ""
	}
	var projects []string
	filters := state.Filters
	switch scope := src.view.Scope; {
	case scope == viewScopeMine:
		filters = append(append([]taskDomain.TaskListParam{}, filters...), taskDomain.TaskListParam{ID: "task_assignee_name", Value: []any{readerUID}})
	case strings.HasPrefix(scope, viewScopeProject):
		id, err := uuid.Parse(strings.TrimPrefix(scope, viewScopeProject))
		if err != nil {
			return bare, viewBroken, ""
		}
		projects = []string{id.String()}
	default:
		return bare, viewBroken, ""
	}
	if len(q.Projects) > 0 && (len(projects) != 1 || projects[0] != q.Projects[0]) {
		return bare, viewUnreadable, ""
	}
	bare.Projects, bare.Filters = projects, filters
	return bare, "", src.view.Name
}

// checkView refuses a view the keeper by can't put on d: one that isn't
// theirs to read, or isn't live, or, on a project's dashboard, isn't one of
// that project's views. The refusal never says which.
func checkView(ctx context.Context, d Dashboard, view string, by uuid.UUID) error {
	return checkViews(ctx, d, []Widget{{Query: Query{View: view}}}, by)
}

// checkViews is checkView for each of ws over a saved view, their views read
// in one go (readViews); ws over none read nothing.
func checkViews(ctx context.Context, d Dashboard, ws []Widget, by uuid.UUID) error {
	var over []Widget
	for _, w := range ws {
		if w.Query.View != "" {
			over = append(over, w)
		}
	}
	if len(over) == 0 {
		return nil
	}
	src, err := readViews(ctx, Reader{UUID: by}, over)
	if err != nil {
		return err
	}
	for _, w := range over {
		q := Query{View: w.Query.View}
		if d.Scope == ScopeProject {
			q.Projects = []string{d.ProjectID}
		}
		if _, msg, _ := fromView(q, src[w.Query.View], ""); msg != "" {
			if d.Scope == ScopeProject {
				return refuse("Choose one of your saved views of this project.")
			}
			return refuse("Choose one of your saved views.")
		}
	}
	return nil
}
