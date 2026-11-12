package business

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/google/uuid"
)

func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// The built-in ids here are the ones the migrations write: the first six from
// task_keys_and_types, Epic from task_type_epic. A type a migration made that
// this package doesn't know, or the other way round, would be a type nobody
// can pick. Each is found by its name, not its number, which is given at
// landing.
func TestTheBuiltInTypesAreTheMigrationsOwn(t *testing.T) {
	var sql []byte
	for _, name := range []string{"task_keys_and_types", "task_type_epic"} {
		files, err := filepath.Glob("../../migrations/*_" + name + ".up.sql")
		if err != nil || len(files) != 1 {
			t.Fatalf("want one %s migration, found %v (%v)", name, files, err)
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		sql = append(sql, b...)
	}
	rows := regexp.MustCompile(`\('([0-9a-f-]{36})', '(\w+)',`).FindAllStringSubmatch(string(sql), -1)
	want := map[string]uuid.UUID{"task": TypeTask, "bug": TypeBug, "feature": TypeFeature, "milestone": TypeMilestone, "approval": TypeApproval, "request": TypeRequest, "epic": TypeEpic}
	if len(rows) != len(want) || len(Builtins()) != len(want) {
		t.Fatalf("the migrations have %d built-in types and Builtins %d, want %d", len(rows), len(Builtins()), len(want))
	}
	for _, r := range rows {
		if id, ok := want[r[2]]; !ok || id.String() != r[1] {
			t.Errorf("the migration's %s is %s; this package's is %s", r[2], r[1], id)
		}
		if !Builtin(uuid.MustParse(r[1])) {
			t.Errorf("%s isn't counted as built in", r[2])
		}
	}
	if Builtin(uuid.New()) {
		t.Error("a project's own type counted as built in")
	}
}

func TestATypeIsANameAndAnIconOfThoseOffered(t *testing.T) {
	name, icon, err := Clean("  Design   review ", "palette")
	if err != nil || name != "Design review" || icon != "palette" {
		t.Errorf("Clean = %q %q %v", name, icon, err)
	}
	for _, c := range []struct{ name, icon, words string }{
		{"", "palette", "Give the type a name."},
		{strings.Repeat("x", MaxNameLen+1), "palette", "at most 40 characters"},
		{"Spike", "skull", "Pick one of the icons offered."},
	} {
		_, _, err := Clean(c.name, c.icon)
		if !errors.Is(err, ErrBadType) || !strings.Contains(err.Error(), c.words) {
			t.Errorf("Clean(%q, %q) = %v; want %q", c.name, c.icon, err, c.words)
		}
	}
}

func TestAProjectsTypesKeepTheirLimits(t *testing.T) {
	project := uuid.New()
	own := make([]model.Type, 0, MaxTypes)
	for i := 0; i < MaxTypes; i++ {
		own = append(own, model.Type{ID: uuid.New(), Name: "T" + strings.Repeat("x", i), ProjectID: &project})
	}
	swap(t, &listTypes, func(context.Context, uuid.UUID, bool) ([]model.Type, error) {
		return append([]model.Type{{ID: TypeBug, Builtin: "bug", Name: "Bug"}}, own...), nil
	})
	made := false
	swap(t, &createType, func(context.Context, uuid.UUID, string, string, uuid.UUID) (model.Type, error) {
		made = true
		return model.Type{}, nil
	})
	if _, err := Create(context.Background(), project, "bug", "bug", uuid.Nil); !errors.Is(err, ErrTypeNameTaken) {
		t.Errorf("a name the built-ins have: %v", err)
	}
	if _, err := Create(context.Background(), project, "Spike", "flask", uuid.Nil); !errors.Is(err, ErrBadType) {
		t.Errorf("one past the limit: %v", err)
	}
	if made {
		t.Error("a refused type was made")
	}
	if err := Update(context.Background(), project, TypeBug, "Defect", "bug"); !errors.Is(err, ErrBadType) {
		t.Errorf("renaming a built-in type: %v", err)
	}
	if err := Archive(context.Background(), project, TypeMilestone); !errors.Is(err, ErrBadType) {
		t.Errorf("archiving a built-in type: %v", err)
	}
}

// A change of type is one history line naming both types; setting the type
// a task already has writes none, and picking Task stores none.
func TestATypeChangeIsWrittenIntoTheTasksHistory(t *testing.T) {
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString()}
	by := &dgraphStruct.DgraphUser{Uid: "0x9"}
	bug := &model.Type{ID: TypeBug, Builtin: "bug", Name: "Bug", Icon: "bug"}
	var stored *uuid.UUID
	current := (*model.Type)(nil)
	swap(t, &setTaskType, func(_ context.Context, _ uuid.UUID, id *uuid.UUID) (*model.Type, *model.Type, error) {
		stored = id
		before := current
		if id == nil {
			current = nil
		} else {
			current = bug
		}
		return before, current, nil
	})
	var lines []*dgraphStruct.DgraphTaskActivity
	swap(t, &writeLine, func(_ context.Context, t *dgraphStruct.DgraphTask, _ []string) error {
		lines = append(lines, t.Activity...)
		return nil
	})
	if _, err := SetTaskType(context.Background(), task, &TypeBug, by); err != nil {
		t.Fatal(err)
	}
	if _, err := SetTaskType(context.Background(), task, &TypeBug, by); err != nil {
		t.Fatal(err)
	}
	taskType := TypeTask
	got, err := SetTaskType(context.Background(), task, &taskType, by)
	if err != nil || got != nil || stored != nil {
		t.Errorf("picking Task: answered %+v, stored %v, %v; want none stored", got, stored, err)
	}
	if len(lines) != 2 {
		t.Fatalf("%d history lines, want 2 (the repeat writes none)", len(lines))
	}
	if l := lines[0]; l.Type != dgraphStruct.ACTIVITY_TYPE_WORK_TYPE || l.PrevState != "Task" || l.NextState != "Bug" || l.CreatedBy.Uid != "0x9" {
		t.Errorf("the first line: %+v", l)
	}
	if l := lines[1]; l.PrevState != "Bug" || l.NextState != "Task" {
		t.Errorf("the second line: %+v", l)
	}
}

// A project holds at most MaxEpics epics (CR-B36 F2): a task made an epic
// with its project at the cap is refused in words and keeps its type; one
// made an epic below the cap becomes one.
func TestAnEpicPastTheProjectsCapIsRefused(t *testing.T) {
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString()}
	by := &dgraphStruct.DgraphUser{Uid: "0x9"}
	project := uuid.New()
	swap(t, &factsFor, func(context.Context, []uuid.UUID) (map[uuid.UUID]model.Facts, error) { return nil, nil })
	swap(t, &placeOf, func(_ context.Context, id string) (taskDomain.Place, error) {
		return taskDomain.Place{UUID: id, Project: project.String()}, nil
	})
	held := 0
	swap(t, &epicsIn, func(_ context.Context, projects []uuid.UUID) ([]string, error) {
		if len(projects) != 1 || projects[0] != project {
			t.Errorf("read the epics of %v, want the task's project", projects)
		}
		return make([]string, held), nil
	})
	stored := 0
	swap(t, &setTaskType, func(_ context.Context, _ uuid.UUID, id *uuid.UUID) (*model.Type, *model.Type, error) {
		stored++
		return nil, &model.Type{ID: *id, Builtin: "epic", Name: "Epic", Icon: "epic"}, nil
	})
	swap(t, &writeLine, func(context.Context, *dgraphStruct.DgraphTask, []string) error { return nil })
	held = MaxEpics
	if _, err := SetTaskType(context.Background(), task, &TypeEpic, by); !errors.Is(err, ErrBadType) || !strings.Contains(err.Error(), "at most") || stored != 0 {
		t.Errorf("at the cap: %v, %d stored; want refused in words, nothing stored", err, stored)
	}
	held = MaxEpics - 1
	if _, err := SetTaskType(context.Background(), task, &TypeEpic, by); err != nil || stored != 1 {
		t.Errorf("below the cap: %v, %d stored; want it made an epic", err, stored)
	}
}

// A type reads the same wherever the app gets it: a built-in one by its key, a project's own marked, and no types
// an empty list rather than null (the template manager and the project's types both read these).
func TestAnswersReadTypesAsTheAppDoes(t *testing.T) {
	project := uuid.New()
	got := Answers([]model.Type{
		{ID: TypeBug, Builtin: "bug", Name: "Bug", Icon: "bug"},
		{ID: project, Name: "Incident", Icon: "flag", ProjectID: &project},
	})
	want := []Answer{
		{ID: TypeBug.String(), Builtin: "bug", Name: "Bug", Icon: "bug"},
		{ID: project.String(), Name: "Incident", Icon: "flag", Own: true},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Answers = %+v, want %+v", got, want)
	}
	if none := Answers(nil); none == nil || len(none) != 0 {
		t.Fatalf("no types read as %#v, want an empty list", none)
	}
}

// The list's and the board's filter by type (linear-03) names typed tasks
// only: the chosen types' tasks, or, with Task chosen, the typed tasks of the
// other types to leave out. Plain tasks are never listed, so a project of
// plain work never sends the graph a list of every task.
func TestTheTypeFilterNamesOnlyTypedTasks(t *testing.T) {
	project, spike := uuid.New(), uuid.New()
	var asked struct {
		types   []uuid.UUID
		outside bool
		calls   int
	}
	held := map[bool][]string{}
	swap(t, &typedTasks, func(_ context.Context, p uuid.UUID, types []uuid.UUID, outside bool) ([]string, error) {
		if p != project {
			t.Fatalf("read project %s, want %s", p, project)
		}
		asked.types, asked.outside, asked.calls = types, outside, asked.calls+1
		return held[outside], nil
	})
	nothing := `eq(task_uuid, "00000000-0000-0000-0000-000000000000")`
	for _, c := range []struct {
		what    string
		ids     []string
		inside  []string
		outside []string
		want    string
		types   []uuid.UUID
		reads   int
	}{
		{"Bug", []string{TypeBug.String()}, []string{"t1", "t2"}, nil, `eq(task_uuid, ["t1","t2"])`, []uuid.UUID{TypeBug}, 1},
		{"Bug and a project's own", []string{TypeBug.String(), spike.String()}, []string{"t1"}, nil, `eq(task_uuid, ["t1"])`, []uuid.UUID{TypeBug, spike}, 1},
		{"a type with no tasks", []string{TypeFeature.String()}, nil, nil, nothing, []uuid.UUID{TypeFeature}, 1},
		{"Task", []string{TypeTask.String()}, nil, []string{"t3"}, `NOT eq(task_uuid, ["t3"])`, nil, 1},
		{"Task and Bug", []string{TypeTask.String(), TypeBug.String()}, nil, []string{"t3"}, `NOT eq(task_uuid, ["t3"])`, []uuid.UUID{TypeBug}, 1},
		{"Task with no typed work", []string{TypeTask.String()}, nil, nil, "", nil, 1},
		{"ids that are no type", []string{"bug", "", "1; drop"}, []string{"t1"}, []string{"t3"}, nothing, nil, 0},
	} {
		held[false], held[true], asked.calls = c.inside, c.outside, 0
		got, err := FilterClause(context.Background(), project, c.ids)
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.what, got, err, c.want)
		}
		if asked.calls != c.reads {
			t.Errorf("%s: %d reads, want %d", c.what, asked.calls, c.reads)
			continue
		}
		plain := c.what == "Task" || strings.HasPrefix(c.what, "Task ")
		if c.reads > 0 && (asked.outside != plain || len(asked.types) != len(c.types)) {
			t.Errorf("%s: read types %v outside %v; want %v outside %v", c.what, asked.types, asked.outside, c.types, plain)
		}
	}
	if FilterID != "task_work_type" {
		t.Errorf("the filter's id is %q; the web sends task_work_type", FilterID)
	}
}
