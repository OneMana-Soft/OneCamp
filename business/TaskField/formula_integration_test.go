//go:build integration

package business

// J-F (jira-01): formula task fields at read, against Postgres 12 with every
// migration. MergeFieldValues works each task's formulas out from its custom
// fields and its built-ins (estimate, points, start, due, status, priority, created),
// under one budget per read, for the reader in ctx: a bare context is nobody,
// an admin of nothing, so money and every formula that reads it stay hidden.
// Who is an admin, through the handlers: tests/integration/projects_jf_formula_test.go.
// Run: go test -tags=integration ./business/TaskField/ -run TestJF -v

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

type jfWorld struct {
	t       *testing.T
	ctx     context.Context
	admin   uuid.UUID
	project uuid.UUID
}

func newJFWorld(t *testing.T) *jfWorld {
	t.Helper()
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	w := &jfWorld{t: t, ctx: ctx, admin: uuid.New(), project: uuid.New()}
	team := uuid.New()
	w.exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'lead@acme.test', 'lead', 'Lead', NOW(), NOW())`, w.admin)
	w.exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Web', $2)`, team, w.admin)
	w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Website', $2, $3)`, w.project, team, w.admin)
	return w
}

func (w *jfWorld) exec(q string, args ...any) {
	w.t.Helper()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(w.ctx, q, args...); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
}

// field makes a field from what the app sends, or fails the test.
func (w *jfWorld) field(sent string) *model.Field {
	w.t.Helper()
	var in Input
	if err := json.Unmarshal([]byte(sent), &in); err != nil {
		w.t.Fatal(err)
	}
	f, err := Create(w.ctx, w.project, in, w.admin, "")
	if err != nil {
		w.t.Fatalf("make %s: %v", sent, err)
	}
	return f
}

// task is a task of the project in Postgres; the read is given it as the
// graph gives it (t), its project said or not.
func (w *jfWorld) task(t *dgraphStruct.DgraphTask) *dgraphStruct.DgraphTask {
	w.t.Helper()
	id := uuid.New()
	w.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, id, w.project, w.admin)
	t.Uuid = id.String()
	return t
}

func (w *jfWorld) set(task *dgraphStruct.DgraphTask, f *model.Field, v any) {
	w.t.Helper()
	b, _ := json.Marshal(v)
	if err := model.SetValue(w.ctx, uuid.MustParse(task.Uuid), f.ID, b, w.admin); err != nil {
		w.t.Fatalf("%s: %v", f.Name, err)
	}
}

// value is a task's value of a field as the read laid it, decoded; ok says it has one.
func jfValue(t *dgraphStruct.DgraphTask, f *model.Field) (any, bool) {
	raw, ok := t.Fields[f.ID.String()]
	if !ok {
		return nil, false
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return v, true
}

func TestJFFormulasAtRead(t *testing.T) {
	w := newJFWorld(t)
	ctx := w.ctx
	hours := w.field(`{"name":"Hours","type":"number"}`)
	channel := w.field(`{"name":"Channel","type":"select","options":[{"label":"Blog"},{"label":"Email"}]}`)
	budget := w.field(`{"name":"Budget","type":"money","currency":"USD"}`)
	double := w.field(`{"name":"Double","type":"formula","formula":"{Hours} * 2"}`)
	span := w.field(`{"name":"Span","type":"formula","formula":"DATETIME_DIFF({Due date}, {Start date}, 'days')"}`)
	effort := w.field(`{"name":"Effort","type":"formula","formula":"{Estimate} * 2"}`)
	size := w.field(`{"name":"Size","type":"formula","formula":"{Points} + 1"}`)
	says := w.field(`{"name":"Says","type":"formula","formula":"{Status} & \" / \" & {Priority} & \" / \" & {Channel}"}`)
	quadruple := w.field(`{"name":"Quadruple","type":"formula","formula":"{Double} * 2"}`)
	spend := w.field(`{"name":"Spend","type":"formula","formula":"{Budget} * 2"}`)
	spendMore := w.field(`{"name":"Spend more","type":"formula","formula":"{Spend} + {Hours}"}`)

	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	due := time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC)
	minutes, points := 90, 3.0
	review, reviewName := "c0ffee00", "Review"
	project := &dgraphStruct.DgraphProject{Uuid: w.project.String()}
	a := w.task(&dgraphStruct.DgraphTask{Project: project, StartDate: &start, DueDate: &due, EstimateMinutes: &minutes, EstimatePoints: &points,
		Priority: "high", Status: "in_progress", CustomStatus: &review, CustomStatusName: &reviewName})
	// b says no project, as a project's list gives its tasks: the read finds it.
	b := w.task(&dgraphStruct.DgraphTask{Priority: "low"})
	w.set(a, hours, 3)
	w.set(a, channel, channel.Options[0].ID)
	w.set(a, budget, 125000)
	w.set(b, hours, 10)

	t.Run("custom fields and built-ins are worked out at read", func(t *testing.T) {
		tasks := []*dgraphStruct.DgraphTask{a, b}
		if err := MergeFieldValues(ctx, tasks); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			task *dgraphStruct.DgraphTask
			f    *model.Field
			want any
		}{
			{a, double, 6.0}, {a, quadruple, 12.0}, {a, span, 7.0}, {a, effort, 3.0}, {a, size, 4.0},
			{a, says, "Review / High / Blog"}, {b, double, 20.0},
		} {
			if got, ok := jfValue(c.task, c.f); !ok || got != c.want {
				t.Errorf("%s on %s: %v (%v), want %v; fields %s", c.f.Name, c.task.Uuid, got, ok, c.want, jfDump(c.task))
			}
		}
		// The stored values are laid on as before.
		if got, _ := jfValue(a, hours); got != 3.0 {
			t.Errorf("Hours: %v", got)
		}
	})

	t.Run("money and what reads it are hidden from nobody's read", func(t *testing.T) {
		tasks := []*dgraphStruct.DgraphTask{a}
		if err := MergeFieldValues(ctx, tasks); err != nil {
			t.Fatal(err)
		}
		for _, f := range []*model.Field{budget, spend, spendMore} {
			if v, ok := jfValue(a, f); ok {
				t.Errorf("%s reached a read for nobody: %v", f.Name, v)
			}
		}
		if got, ok := jfValue(a, double); !ok || got != 6.0 {
			t.Errorf("Double reads no money and stays: %v %v", got, ok)
		}
	})

	t.Run("a formula's value is never written", func(t *testing.T) {
		_, err := SetValue(ctx, a, double, json.RawMessage(`5`), &dgraphStruct.DgraphUser{}, w.admin)
		var in *InputError
		if !errors.As(err, &in) || !strings.Contains(err.Error(), "worked out") {
			t.Errorf("setting Double: %v", err)
		}
		var n int
		if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_field_values WHERE field_id = $1`, double.ID).Scan(&n); err != nil || n != 0 {
			t.Errorf("Double has %d stored values (%v)", n, err)
		}
	})

	t.Run("a formula that loops, or reads what isn't there, is refused", func(t *testing.T) {
		for _, src := range []string{"{Nothing} + 1", "{Hours} +", "{Loop} + 1"} {
			sent, _ := json.Marshal(map[string]any{"name": "Loop", "type": "formula", "formula": src})
			var in Input
			_ = json.Unmarshal(sent, &in)
			if _, err := Create(ctx, w.project, in, w.admin, ""); !errors.As(err, new(*InputError)) {
				t.Errorf("make Loop = %s: %v", src, err)
			}
		}
		// Double reads Hours; Quadruple reads Double. Double reading Quadruple loops.
		var in Input
		_ = json.Unmarshal([]byte(`{"name":"Double","type":"formula","formula":"{Quadruple} * 2"}`), &in)
		_, _, err := Update(ctx, w.project, double.ID, in)
		if !errors.As(err, new(*InputError)) || !strings.Contains(err.Error(), "its own value") {
			t.Errorf("Double reading Quadruple: %v", err)
		}
	})

	t.Run("a deleted field's formula says so, and its restore brings the value back", func(t *testing.T) {
		if err := Delete(ctx, w.project, hours.ID, w.admin); err != nil {
			t.Fatal(err)
		}
		tasks := []*dgraphStruct.DgraphTask{a}
		_ = MergeFieldValues(ctx, tasks)
		got, _ := jfValue(a, double)
		if m, _ := got.(map[string]any); m == nil || !strings.Contains(m["error"].(string), "deleted") {
			t.Errorf("Double over a deleted field: %v", got)
		}
		if err := Restore(ctx, w.project, hours.ID); err != nil {
			t.Fatal(err)
		}
		_ = MergeFieldValues(ctx, tasks)
		if got, _ := jfValue(a, double); got != 6.0 {
			t.Errorf("Double after Hours' restore: %v", got)
		}
	})
}

// A read's formulas share one budget: a page too heavy to work out gives the
// tasks past the budget an error, never a hung server, and the next read
// starts afresh.
func TestJFAReadHasOneBudget(t *testing.T) {
	w := newJFWorld(t)
	notes := w.field(`{"name":"Notes","type":"text"}`)
	src := strings.TrimSuffix(strings.Repeat("LEN(UPPER(REPT({Notes},4)))+", 70), "+")
	heavy := w.field(`{"name":"Heavy","type":"formula","formula":"` + src + `"}`)
	text := strings.Repeat("abcdefgh", 250)
	var tasks []*dgraphStruct.DgraphTask
	for i := 0; i < 400; i++ {
		tasks = append(tasks, w.task(&dgraphStruct.DgraphTask{Project: &dgraphStruct.DgraphProject{Uuid: w.project.String()}}))
	}
	w.exec(`INSERT INTO task_field_values (task_uuid, field_id, value) SELECT id, $1, to_jsonb($2::text) FROM tasks WHERE project_id = $3`, notes.ID, text, w.project)
	if err := MergeFieldValues(w.ctx, tasks); err != nil {
		t.Fatal(err)
	}
	want := float64(70 * 4 * len(text))
	if got, _ := jfValue(tasks[0], heavy); got != want {
		t.Fatalf("the first task: %v, want %v", got, want)
	}
	ranOut := false
	for i, task := range tasks {
		got, _ := jfValue(task, heavy)
		_, isErr := got.(map[string]any)
		if ranOut && !isErr {
			t.Fatalf("task %d worked out after the budget ran out: %v", i, got)
		}
		ranOut = ranOut || isErr
	}
	if !ranOut {
		t.Fatal("400 heavy tasks fit one read's budget")
	}
	last := []*dgraphStruct.DgraphTask{tasks[len(tasks)-1]}
	if err := MergeFieldValues(w.ctx, last); err != nil {
		t.Fatal(err)
	}
	if got, _ := jfValue(last[0], heavy); got != want {
		t.Errorf("a read of its own: %v", got)
	}
}

func jfDump(t *dgraphStruct.DgraphTask) string {
	b, _ := json.Marshal(t.Fields)
	return string(b)
}
