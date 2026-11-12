package business

// J-F's read without a database: a formula is checked against its project's
// fields as it's saved (compileFormula), and a read's formulas are worked out
// under one budget (workOut). Postgres's part: formula_integration_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	formula "github.com/akashc777/OneCamp/business/Formula"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

func jfField(project uuid.UUID, name, typ, src string) *model.Field {
	return &model.Field{ID: uuid.New(), ProjectID: project, Name: name, Type: typ, Formula: src}
}

// A formula that reads itself, directly or through another, or a field that
// isn't there, is refused in the Formula's words; one that reads fields and
// built-ins is kept with its fields by id.
func TestAFormulaIsCheckedAgainstItsFields(t *testing.T) {
	p := uuid.New()
	hours := jfField(p, "Hours", TypeNumber, "")
	double := jfField(p, "Double", TypeFormula, "{Hours} * 2")
	quad := jfField(p, "Quad", TypeFormula, "{Double} * 2")
	fields := []*model.Field{hours, double, quad}

	kind, stored, err := compileFormula(fields, uuid.Nil, "Span", "DATETIME_DIFF({Due date}, {Start date}, 'days') + {Hours}")
	if err != nil || kind != formula.KindNumber || !strings.Contains(stored, "{#"+hours.ID.String()+"}") || !strings.Contains(stored, "{#due}") {
		t.Errorf("Span: %v %q %v", kind, stored, err)
	}
	for _, c := range []struct {
		self            *model.Field
		name, src, want string
	}{
		{double, "Double", "{Quad} * 2", "its own value"},
		{nil, "Loop", "{Loop} + 1", "its own value"},
		{nil, "Bad", "{Nothing} + 1", "Nothing"},
	} {
		self := uuid.Nil
		if c.self != nil {
			self = c.self.ID
		}
		_, _, err := compileFormula(fields, self, c.name, c.src)
		var in *InputError
		if !errors.As(err, &in) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s = %s: %v", c.name, c.src, err)
		}
	}
}

// One read's formulas share one budget: past it, every task's formula says
// so (never a hung server), and the next read starts afresh.
func TestAReadWorksFormulasOutUnderOneBudget(t *testing.T) {
	p := uuid.New()
	notes := jfField(p, "Notes", TypeText, "")
	src := strings.TrimSuffix(strings.Repeat("LEN(UPPER(REPT({Notes},4)))+", 70), "+")
	heavy := jfField(p, "Heavy", TypeFormula, src)
	fields := []*model.Field{notes, heavy}
	text, _ := json.Marshal(strings.Repeat("abcdefgh", 250))
	read := func(n int) []*dgraphStruct.DgraphTask {
		tasks := make([]*dgraphStruct.DgraphTask, n)
		values, projects := model.Values{}, map[string]string{}
		for i := range tasks {
			tasks[i] = &dgraphStruct.DgraphTask{Uuid: uuid.NewString()}
			values[tasks[i].Uuid] = map[string]json.RawMessage{notes.ID.String(): text}
			projects[tasks[i].Uuid] = p.String()
		}
		workOut(context.Background(), tasks, values, projects, fields, nil)
		for _, task := range tasks {
			task.Fields = values[task.Uuid]
		}
		return tasks
	}
	value := func(task *dgraphStruct.DgraphTask) string { return string(task.Fields[heavy.ID.String()]) }
	tasks := read(400)
	if value(tasks[0]) != "560000" {
		t.Fatalf("the first task: %s", value(tasks[0]))
	}
	ranOut := false
	for i, task := range tasks {
		isErr := strings.Contains(value(task), formula.RanOut().Str)
		if ranOut && !isErr {
			t.Fatalf("task %d worked out after the budget ran out: %s", i, value(task))
		}
		ranOut = ranOut || isErr
	}
	if !ranOut {
		t.Fatal("400 heavy tasks fit one read's budget")
	}
	if got := value(read(1)[0]); got != "560000" {
		t.Errorf("a read of its own: %s", got)
	}
}
