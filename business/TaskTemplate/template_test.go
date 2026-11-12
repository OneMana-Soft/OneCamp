package business

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	projectTemplate "github.com/akashc777/OneCamp/business/ProjectTemplate"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

func kept(t *testing.T, tpl Template) Template {
	t.Helper()
	got, err := Check(tpl)
	if err != nil {
		t.Fatalf("Check(%q): %v", tpl.Name, err)
	}
	return got
}

func days(n int) *int { return &n }

// A template is kept tidy: its own name and its sub-tasks' trimmed (the
// task's name is kept as typed: TestATasksNameIsKeptAsTyped), required
// properties kept one way and once each, and a default for a type has that type.
func TestATemplateIsTidiedAsItIsKept(t *testing.T) {
	bug, field := workTaxonomy.TypeBug.String(), uuid.New()
	got := kept(t, Template{Name: "  Bug report ", DefaultFor: strings.ToUpper(bug), Shape: Shape{
		Task: projectTemplate.Task{Name: "[Bug] ", Description: "<p>Steps to reproduce</p>", Priority: "high", Tags: "bug",
			DueDay: days(3), Subtasks: []projectTemplate.Subtask{{Name: " Reproduce "}, {Name: "Fix"}}},
		Required: []string{" Priority", "field:" + strings.ToUpper(field.String()), "priority"},
	}})
	if got.Name != "Bug report" || got.Shape.Name != "[Bug] " || got.Shape.Subtasks[0].Name != "Reproduce" {
		t.Errorf("names kept as %q, %q, %q", got.Name, got.Shape.Name, got.Shape.Subtasks[0].Name)
	}
	if want := []string{taskWriter.PropPriority, taskWriter.PropField + field.String()}; !slices.Equal(got.Shape.Required, want) {
		t.Errorf("required kept as %v, want %v", got.Shape.Required, want)
	}
	if got.DefaultFor != bug || got.Shape.Type != bug {
		t.Errorf("a default for Bug kept as default %q, type %q", got.DefaultFor, got.Shape.Type)
	}
}

// A task's name in a template is where the name of a task made from it
// starts: the maker types after "Bug: ", so the name is kept exactly as typed,
// and one of only spaces is none.
func TestATasksNameIsKeptAsTyped(t *testing.T) {
	for typed, want := range map[string]string{"Bug: ": "Bug: ", " [Bug] ": " [Bug] ", "Bug report": "Bug report", " \t ": "", "": ""} {
		got := kept(t, Template{Name: "Bug report", Shape: Shape{Task: projectTemplate.Task{Name: typed}}})
		if got.Shape.Name != want {
			t.Errorf("a task's name typed %q is kept as %q, want %q", typed, got.Shape.Name, want)
		}
	}
}

// A template that can't be kept as it is says what to fix, in words.
func TestATemplateThatCantBeKeptIsRefusedInWords(t *testing.T) {
	bug, feature := workTaxonomy.TypeBug.String(), workTaxonomy.TypeFeature.String()
	many := make([]projectTemplate.Subtask, MaxSubtasks+1)
	for i := range many {
		many[i].Name = "Step"
	}
	for _, c := range []struct {
		name  string
		tpl   Template
		words string
	}{
		{"no name", Template{Name: "  "}, "needs a name"},
		{"a long name", Template{Name: strings.Repeat("a", MaxNameLength+1)}, "60"},
		{"a long description", Template{Name: "x", Description: strings.Repeat("a", MaxAboutLength+1)}, "280"},
		{"an unknown priority", Template{Name: "x", Shape: Shape{Task: projectTemplate.Task{Priority: "whenever"}}}, "whenever"},
		{"an unknown property asked for", Template{Name: "x", Shape: Shape{Required: []string{"mood"}}}, "mood"},
		{"a field that isn't an id", Template{Name: "x", Shape: Shape{Fields: map[string]json.RawMessage{"size": json.RawMessage(`"L"`)}}}, "size"},
		{"a type that isn't an id", Template{Name: "x", Shape: Shape{Type: "bug"}}, "type"},
		{"an assignee who isn't an id", Template{Name: "x", Shape: Shape{Assignee: "sam"}}, "assignee"},
		{"a default for no type", Template{Name: "x", DefaultFor: "bug"}, "type"},
		{"a default for a type it doesn't have", Template{Name: "x", DefaultFor: bug, Shape: Shape{Type: feature}}, "type"},
		{"too many sub-tasks", Template{Name: "x", Shape: Shape{Task: projectTemplate.Task{Subtasks: many}}}, "sub-tasks"},
		{"a sub-task with no name", Template{Name: "x", Shape: Shape{Task: projectTemplate.Task{Subtasks: []projectTemplate.Subtask{{Name: " "}}}}}, "sub-task"},
		{"a sub-task with a day", Template{Name: "x", Shape: Shape{Task: projectTemplate.Task{Subtasks: []projectTemplate.Subtask{{Name: "a", DueDay: days(2)}}}}}, "day"},
		{"due before it starts", Template{Name: "x", Shape: Shape{Task: projectTemplate.Task{StartDay: days(5), DueDay: days(2)}}}, "before"},
		{"a day past ten years", Template{Name: "x", Shape: Shape{Task: projectTemplate.Task{DueDay: days(projectTemplate.MaxDay + 1)}}}, "day"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Check(c.tpl)
			var te *TemplateError
			if !errors.As(err, &te) || !strings.Contains(err.Error(), c.words) {
				t.Fatalf("Check = %v, want a TemplateError saying %q", err, c.words)
			}
		})
	}
}

func fitFacts() (Facts, *taskFieldModel.Field, *taskFieldModel.Field) {
	size := &taskFieldModel.Field{ID: uuid.New(), Name: "Size", Type: taskFieldModel.TypeText}
	points := &taskFieldModel.Field{ID: uuid.New(), Name: "Points", Type: taskFieldModel.TypeNumber}
	return Facts{
		Fields:     []*taskFieldModel.Field{size, points},
		Statuses:   []string{"todo", "in_progress", "done", "Review"},
		Assignable: map[string]bool{},
	}, size, points
}

func str(s string) *string { return &s }

// A task made from a template takes its presets, its days counted from the
// day it is made, and its sub-tasks in order; what the maker typed wins over
// a preset, and typing nothing over one clears it.
func TestAMadeTaskTakesThePresetsAndWhatWasTypedOverThem(t *testing.T) {
	facts, size, points := fitFacts()
	sam, ana := uuid.NewString(), uuid.NewString()
	facts.Assignable[sam], facts.Assignable[ana] = true, true
	bug := workTaxonomy.TypeBug
	tpl := kept(t, Template{Name: "Bug report", Shape: Shape{
		Task: projectTemplate.Task{Name: "[Bug] ", Description: "<p>Steps</p>", Status: "review", Priority: "high", Tags: "bug,web",
			StartDay: days(1), DueDay: days(3), Subtasks: []projectTemplate.Subtask{{Name: "Reproduce"}, {Name: "Fix"}, {Name: "Verify"}}},
		Type: bug.String(), Assignee: sam,
		Fields: map[string]json.RawMessage{size.ID.String(): json.RawMessage(`"L"`), points.ID.String(): json.RawMessage(`3`)},
	}})
	start := projectTemplate.Start{Day: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Loc: time.UTC}

	made := Fit(tpl, Typed{}, facts, start)
	task := made.Task
	if task.Name != "[Bug]" || task.Description != "<p>Steps</p>" || task.Status != "review" || task.Priority != "high" || task.Label != "bug,web" {
		t.Errorf("presets made %+v", task)
	}
	wantStart, _ := time.Parse(time.RFC3339, start.At(1, 9))
	wantDue, _ := time.Parse(time.RFC3339, start.At(3, 17))
	if task.Start == nil || !task.Start.Equal(wantStart) || task.Due == nil || !task.Due.Equal(wantDue) {
		t.Errorf("days made start %v and due %v, want %v and %v", task.Start, task.Due, wantStart, wantDue)
	}
	if task.Type == nil || *task.Type != bug || task.Assignee == nil || task.Assignee.Uuid != sam {
		t.Errorf("type %v and assignee %+v, want Bug and sam", task.Type, task.Assignee)
	}
	if string(task.Fields[size.ID]) != `"L"` || string(task.Fields[points.ID]) != `3` || len(task.Fields) != 2 {
		t.Errorf("fields made %s", fieldsOf(task.Fields))
	}
	if !slices.Equal(made.Subtasks, []string{"Reproduce", "Fix", "Verify"}) || len(made.LeftOff) != 0 {
		t.Errorf("sub-tasks %v, left off %v", made.Subtasks, made.LeftOff)
	}

	due := time.Date(2026, 11, 20, 17, 0, 0, 0, time.UTC)
	made = Fit(tpl, Typed{
		Name: str("Checkout crashes on Safari"), Priority: str(""), Assignee: str(ana), Due: &due, ClearStart: true,
		Fields: map[string]json.RawMessage{size.ID.String(): json.RawMessage(`null`)},
	}, facts, start)
	task = made.Task
	if task.Name != "Checkout crashes on Safari" || task.Priority != "" || task.Assignee == nil || task.Assignee.Uuid != ana {
		t.Errorf("typed over: %+v", task)
	}
	if task.Start != nil || task.Due == nil || !task.Due.Equal(due) {
		t.Errorf("typed over: start %v, due %v", task.Start, task.Due)
	}
	if _, ok := task.Fields[size.ID]; ok || string(task.Fields[points.ID]) != `3` {
		t.Errorf("typed over: fields %s", fieldsOf(task.Fields))
	}
}

// A preset the project can't take is left off and named, and the rest of the
// task is still made.
func TestAPresetTheProjectCantTakeIsLeftOffAndNamed(t *testing.T) {
	facts, size, points := fitFacts()
	own := uuid.New()
	facts.Types = []uuid.UUID{own}
	tpl := kept(t, Template{Name: "Request", Shape: Shape{
		Task: projectTemplate.Task{Name: "Request", Status: "Shipping", Priority: "low"},
		Type: uuid.NewString(), Assignee: uuid.NewString(),
		Fields: map[string]json.RawMessage{
			size.ID.String(): json.RawMessage(`"S"`), points.ID.String(): json.RawMessage(`"lots"`), uuid.NewString(): json.RawMessage(`"x"`),
		},
	}})
	made := Fit(tpl, Typed{}, facts, projectTemplate.Start{Day: time.Now().UTC(), Loc: time.UTC})
	if made.Task.Name != "Request" || made.Task.Priority != "low" || string(made.Task.Fields[size.ID]) != `"S"` {
		t.Errorf("what fits wasn't made: %+v", made.Task)
	}
	if made.Task.Assignee != nil || made.Task.Type != nil || made.Task.Status != "" || len(made.Task.Fields) != 1 {
		t.Errorf("what doesn't fit was made: %+v", made.Task)
	}
	words := strings.Join(made.LeftOff, " | ")
	for _, w := range []string{"assignee", "type", "Shipping", "Points", "a field this project doesn't have"} {
		if !strings.Contains(words, w) {
			t.Errorf("left off %q doesn't name %q", words, w)
		}
	}
	if len(made.LeftOff) != 5 {
		t.Errorf("left off %d presets, want 5: %v", len(made.LeftOff), made.LeftOff)
	}
	tpl.Shape.Type = own.String()
	if got := Fit(tpl, Typed{}, facts, projectTemplate.Start{Day: time.Now().UTC(), Loc: time.UTC}).Task.Type; got == nil || *got != own {
		t.Errorf("the project's own type made %v", got)
	}
	// Task is a choice like any other (CR-167 F2): kept, so a template that
	// asks for a type is met by it; the Task Writer sets none for it.
	tpl.Shape.Type = workTaxonomy.TypeTask.String()
	if got := Fit(tpl, Typed{}, facts, projectTemplate.Start{Day: time.Now().UTC(), Loc: time.UTC}).Task.Type; got == nil || *got != workTaxonomy.TypeTask {
		t.Errorf("a Task chosen made %v, want Task", got)
	}
}

// A project's templates are its admins'; the workspace's are the workspace
// admins'. Anyone who can make a task in a project makes one from either, and
// nobody else sees them: not a commenter, a viewer, the team's admins who
// run the project from outside it, or anyone in an archived project.
func TestWhoMayKeepATemplateAndWhoMayMakeATaskFromOne(t *testing.T) {
	project, workspace := Template{ProjectID: uuid.NewString()}, Template{}
	for _, c := range []struct {
		who               string
		s                 workAccess.Standing
		admin             bool
		manage, ws, apply bool
	}{
		{"a project admin", workAccess.Standing{Role: workAccess.RoleAdmin}, false, true, false, true},
		{"a member", workAccess.Standing{Role: workAccess.RoleMember}, false, false, false, true},
		{"a commenter", workAccess.Standing{Role: workAccess.RoleCommenter}, false, false, false, false},
		{"a viewer", workAccess.Standing{Role: workAccess.RoleViewer}, false, false, false, false},
		{"the team's admin outside it", workAccess.Standing{Steward: true}, false, false, false, false},
		{"someone not in it", workAccess.Standing{}, false, false, false, false},
		{"a workspace admin who is a member", workAccess.Standing{Role: workAccess.RoleMember}, true, false, true, true},
		{"an admin of an archived project", workAccess.Standing{Role: workAccess.RoleAdmin, Archived: true}, false, false, false, false},
	} {
		if got := MayManage(project, c.s, c.admin); got != c.manage {
			t.Errorf("%s keeps the project's templates: %v, want %v", c.who, got, c.manage)
		}
		if got := MayManage(workspace, c.s, c.admin); got != c.ws {
			t.Errorf("%s keeps the workspace's templates: %v, want %v", c.who, got, c.ws)
		}
		if got := MayApply(c.s); got != c.apply {
			t.Errorf("%s makes a task from a template: %v, want %v", c.who, got, c.apply)
		}
	}
}

// A task of a type starts from the project's default for it, else the
// workspace's.
func TestATypesDefaultIsTheProjectsThenTheWorkspaces(t *testing.T) {
	p := uuid.NewString()
	bug, feature, request := workTaxonomy.TypeBug.String(), workTaxonomy.TypeFeature.String(), workTaxonomy.TypeRequest.String()
	list := []Summary{
		{ID: "ws-bug", Name: "Bug (workspace)", DefaultFor: bug},
		{ID: "p-plain", Name: "Plain", ProjectID: p},
		{ID: "p-bug", Name: "Bug", ProjectID: p, DefaultFor: bug},
		{ID: "ws-feature", Name: "Feature", DefaultFor: feature},
	}
	got := Defaults(list)
	if len(got) != 2 || got[bug] != "p-bug" || got[feature] != "ws-feature" || got[request] != "" {
		t.Errorf("Defaults = %v, want bug p-bug and feature ws-feature only", got)
	}
	// The project's wins in either order.
	list[0], list[2] = list[2], list[0]
	if got := Defaults(list); got[bug] != "p-bug" {
		t.Errorf("Defaults with the project's first = %v", got)
	}
}

func fieldsOf(m map[uuid.UUID]json.RawMessage) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// A money value is set by the project's admins only (V-698): what anyone
// else types in a money field is left off and named, and the template's own
// value, an admin's, stands; an admin's typed value wins as any other.
func TestOnlyAProjectAdminTypesAMoneyValue(t *testing.T) {
	facts, size, _ := fitFacts()
	budget := &taskFieldModel.Field{ID: uuid.New(), Name: "Budget", Type: taskFieldModel.TypeMoney}
	cost := &taskFieldModel.Field{ID: uuid.New(), Name: "Cost", Type: taskFieldModel.TypeMoney}
	facts.Fields = append(facts.Fields, budget, cost)
	tpl := kept(t, Template{Name: "Spend", Shape: Shape{Task: projectTemplate.Task{Name: "Spend"},
		Fields: map[string]json.RawMessage{budget.ID.String(): json.RawMessage(`5000`)}}})
	typed := Typed{Fields: map[string]json.RawMessage{
		budget.ID.String(): json.RawMessage(`99`), cost.ID.String(): json.RawMessage(`42`), size.ID.String(): json.RawMessage(`"M"`)}}
	start := projectTemplate.Start{Day: time.Now().UTC(), Loc: time.UTC}

	made := Fit(tpl, typed, facts, start)
	if string(made.Task.Fields[budget.ID]) != `5000` || made.Task.Fields[cost.ID] != nil || string(made.Task.Fields[size.ID]) != `"M"` {
		t.Errorf("a member's typed money: %s", fieldsOf(made.Task.Fields))
	}
	if len(made.LeftOff) != 2 || !strings.Contains(made.LeftOff[0], "Budget: only project admins can set money fields") || !strings.Contains(made.LeftOff[1], "Cost") {
		t.Errorf("left off: %v", made.LeftOff)
	}

	facts.Money = true
	made = Fit(tpl, typed, facts, start)
	if string(made.Task.Fields[budget.ID]) != `99` || string(made.Task.Fields[cost.ID]) != `42` || len(made.LeftOff) != 0 {
		t.Errorf("an admin's typed money: %s, left off %v", fieldsOf(made.Task.Fields), made.LeftOff)
	}
}
