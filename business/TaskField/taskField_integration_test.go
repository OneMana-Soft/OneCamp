//go:build integration

package business

// Custom fields against Postgres 12 with every migration, and a real Dgraph
// for the task history a value change writes.
// Run: go test -tags=integration ./business/TaskField/ -v

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestCustomFields(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	person, admin, project := uuid.New(), uuid.New(), uuid.New()
	for i, id := range []uuid.UUID{person, admin} {
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, $2, NOW(), NOW())`,
			id, []string{"maya@example.com", "lead@example.com"}[i]); err != nil {
			t.Fatal(err)
		}
	}
	uids := dg.Mutate(t, map[string]any{"uid": "_:me", "dgraph.type": "User", "user_uuid": admin.String(), "user_name": "Lead"})
	me := &dgraphStruct.DgraphUser{Uid: uids["me"]}
	task := func() *dgraphStruct.DgraphTask {
		id := uuid.New()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO tasks (id) VALUES ($1)`, id); err != nil {
			t.Fatal(err)
		}
		dg.Mutate(t, map[string]any{"dgraph.type": "Task", "task_uuid": id.String(), "task_name": "T", "task_deleted_at": "0001-01-01T00:00:00Z"})
		return &dgraphStruct.DgraphTask{Uuid: id.String()}
	}
	newField := func(in Input) *model.Field {
		f, err := Create(ctx, project, in, admin, "")
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	channel := newField(Input{Name: "Channel", Type: TypeSelect, Options: []OptionInput{{Label: "Blog"}, {Label: "Email"}}})
	areas := newField(Input{Name: "Areas", Type: TypeMultiSelect, Options: []OptionInput{{Label: "Web"}, {Label: "App"}}})
	reviewer := newField(Input{Name: "Reviewer", Type: TypePerson})
	signed := newField(Input{Name: "Signed off", Type: TypeCheckbox})
	budget := newField(Input{Name: "Budget", Type: TypeMoney})
	if budget.Currency != DefaultCurrency || len(channel.Options) != 2 {
		t.Fatalf("money without a currency takes the default: %+v; options %+v", budget, channel.Options)
	}
	if _, err := Create(ctx, project, Input{Name: "channel", Type: TypeText}, admin, ""); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a second field called channel: %v", err)
	}
	blog, email := channel.Options[0].ID, channel.Options[1].ID
	web, app := areas.Options[0].ID, areas.Options[1].ID

	t1, t2, t3 := task(), task(), task()
	set := func(tk *dgraphStruct.DgraphTask, f *model.Field, raw string) {
		t.Helper()
		if _, err := SetValue(ctx, tk, f, json.RawMessage(raw), me, admin); err != nil {
			t.Fatalf("%s = %s: %v", f.Name, raw, err)
		}
	}
	set(t1, channel, `"`+blog+`"`)
	set(t1, areas, `["`+web+`","`+app+`"]`)
	set(t1, reviewer, `"`+person.String()+`"`)
	set(t1, signed, `true`)
	set(t1, budget, `125000`)
	set(t1, budget, `125000`) // the same again writes nothing
	set(t2, channel, `"`+email+`"`)
	var ie *InputError
	if _, err := SetValue(ctx, t2, reviewer, json.RawMessage(`"`+uuid.NewString()+`"`), me, admin); !errors.As(err, &ie) {
		t.Errorf("someone OneCamp doesn't know: %v", err)
	}

	tasks := []*dgraphStruct.DgraphTask{{Uuid: t1.Uuid}, {Uuid: t2.Uuid}, {Uuid: t3.Uuid}}
	if err := MergeFieldValues(ctx, tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks[0].Fields) != 5 || string(tasks[0].Fields[budget.ID.String()]) != "125000" || len(tasks[1].Fields) != 1 || tasks[2].Fields != nil {
		t.Fatalf("values laid over the tasks: %v / %v / %v", tasks[0].Fields, tasks[1].Fields, tasks[2].Fields)
	}

	// Each change is a line in the task's history; setting the same value again isn't.
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, `{ t(func: eq(task_uuid, "`+t1.Uuid+`")) { task_activities @filter(eq(activity_type, "fieldUpdate")) { activity_next_state } } }`)
	if err != nil {
		t.Fatal(err)
	}
	var hist struct {
		T []struct {
			A []struct {
				Next string `json:"activity_next_state"`
			} `json:"task_activities"`
		} `json:"t"`
	}
	_ = json.Unmarshal(resp.GetJson(), &hist)
	if len(hist.T) != 1 || len(hist.T[0].A) != 5 {
		t.Errorf("five changes in the history: %+v", hist)
	}

	clause := func(f *model.Field, values ...string) string {
		t.Helper()
		c, err := FilterClause(ctx, project, FilterID(f.ID), values)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := clause(channel, blog); c != `(eq(task_uuid, ["`+t1.Uuid+`"]))` {
		t.Errorf("channel is blog: %s", c)
	}
	if c := clause(channel, FilterAny); !strings.Contains(c, t1.Uuid) || !strings.Contains(c, t2.Uuid) || strings.Contains(c, t3.Uuid) {
		t.Errorf("channel set: %s", c)
	}
	if c := clause(channel, FilterNone); !strings.HasPrefix(c, "(NOT eq(task_uuid") || strings.Contains(c, t3.Uuid) {
		t.Errorf("channel not set: %s", c)
	}
	if c := clause(channel, email, FilterNone); !strings.Contains(c, " OR ") {
		t.Errorf("email or not set: %s", c)
	}
	if c := clause(areas, app); !strings.Contains(c, t1.Uuid) {
		t.Errorf("areas hold app: %s", c)
	}
	if c := clause(signed, "true"); !strings.Contains(c, t1.Uuid) {
		t.Errorf("signed off: %s", c)
	}
	if c := clause(reviewer, person.String()); !strings.Contains(c, t1.Uuid) {
		t.Errorf("reviewed by Maya: %s", c)
	}
	if c, _ := FilterClause(ctx, uuid.New(), FilterID(channel.ID), []string{blog}); c != "" {
		t.Errorf("another project's field narrows nothing: %s", c)
	}

	// Options taken away come off the tasks that had them.
	_, changed, err := Update(ctx, project, channel.ID, Input{Name: "Channel", Options: []OptionInput{{ID: email, Label: "Email"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != t1.Uuid {
		t.Errorf("blog came off t1: %v", changed)
	}
	if _, changed, err = Update(ctx, project, areas.ID, Input{Name: "Areas", Options: []OptionInput{{ID: app, Label: "App"}}}); err != nil || len(changed) != 1 {
		t.Fatalf("web came off t1: %v %v", changed, err)
	}
	v, _ := model.ValueOf(ctx, uuid.MustParse(t1.Uuid), areas.ID)
	var left []string
	_ = json.Unmarshal(v, &left)
	if len(left) != 1 || left[0] != app {
		t.Errorf("t1's areas keep app: %s", v)
	}
	if _, _, err = Update(ctx, project, areas.ID, Input{Name: "Areas"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := model.ValueOf(ctx, uuid.MustParse(t1.Uuid), areas.ID); v != nil {
		t.Errorf("no options left, no value: %s", v)
	}
	if f, _, err := Update(ctx, project, budget.ID, Input{Name: "Budget", Type: TypeText}); err != nil || f.Type != TypeMoney || f.Currency != DefaultCurrency {
		t.Errorf("a field keeps its type and currency: %+v %v", f, err)
	}

	// A repeating task's next copy takes the values.
	if err := CopyValues(ctx, uuid.MustParse(t1.Uuid), uuid.MustParse(t3.Uuid), admin); err != nil {
		t.Fatal(err)
	}
	copied, _ := model.ValuesFor(ctx, []uuid.UUID{uuid.MustParse(t3.Uuid)})
	var got []string
	for id := range copied[t3.Uuid] {
		got = append(got, id)
	}
	sort.Strings(got)
	want := []string{budget.ID.String(), reviewer.ID.String(), signed.ID.String()}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("copied %v, want %v", got, want)
	}

	// Deleting a field takes its values with it.
	if err := Delete(ctx, project, budget.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ := model.ValueOf(ctx, uuid.MustParse(t1.Uuid), budget.ID); v != nil {
		t.Errorf("the deleted field's value: %s", v)
	}
	if fields, _ := List(ctx, project); len(fields) != 4 {
		t.Errorf("%d fields left, want 4", len(fields))
	}
}

// Options two writers add at the same moment both stay: AddOptions reads
// and writes the field under its lock.
func TestAddOptionsTogether(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	project := uuid.New()
	f, err := Create(ctx, project, Input{Name: "Channel", Type: TypeSelect, Options: []OptionInput{{Label: "Blog"}}}, uuid.New(), "")
	if err != nil {
		t.Fatal(err)
	}
	labels := []string{"Email", "Social", "Podcast", "Video", "blog"}
	var wg sync.WaitGroup
	for _, l := range labels {
		wg.Add(1)
		go func(l string) {
			defer wg.Done()
			if _, err := AddOptions(ctx, project, f.ID, []OptionInput{{Label: l}}); err != nil {
				t.Error(err)
			}
		}(l)
	}
	wg.Wait()
	got, err := model.Get(ctx, project, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range got.Options {
		names = append(names, o.Label)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "Blog,Email,Podcast,Social,Video" || got.Options[0].ID != f.Options[0].ID {
		t.Errorf("every new option once, Blog untouched: %v", names)
	}
	if _, err := AddOptions(ctx, project, uuid.New(), []OptionInput{{Label: "x"}}); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("another project's field: %v", err)
	}
}
