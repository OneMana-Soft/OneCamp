package business

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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

// The six built-in ids here are the ones the task_keys_and_types migration
// writes: a type the migration made that this package doesn't know, or the
// other way round, would be a type nobody can pick. Found by its name, not
// its number, which is given at landing.
func TestTheBuiltInTypesAreTheMigrationsOwn(t *testing.T) {
	files, err := filepath.Glob("../../migrations/*_task_keys_and_types.up.sql")
	if err != nil || len(files) != 1 {
		t.Fatalf("want one task_keys_and_types migration, found %v (%v)", files, err)
	}
	sql, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	rows := regexp.MustCompile(`\('([0-9a-f-]{36})', '(\w+)',`).FindAllStringSubmatch(string(sql), -1)
	want := map[string]uuid.UUID{"task": TypeTask, "bug": TypeBug, "feature": TypeFeature, "milestone": TypeMilestone, "approval": TypeApproval, "request": TypeRequest}
	if len(rows) != len(want) {
		t.Fatalf("the migration has %d built-in types, want %d", len(rows), len(want))
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
