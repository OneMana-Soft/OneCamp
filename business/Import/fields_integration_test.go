//go:build integration

package business

// Imported custom fields against Postgres 12 with every migration.
// Run: go test -tags=integration ./business/Import/ -run TestImportedFields -v

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskField "github.com/akashc777/OneCamp/business/TaskField"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestImportedFields(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	job := &importModels.Job{Id: uuid.New(), Provider: "asana", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusRunning}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	project, by, maya := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'maya@example.com', NOW(), NOW())`, maya)
	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityUser, "u-maya", maya, nil, mustMarshal(map[string]any{}), true); err != nil {
		t.Fatal(err)
	}
	// Someone made a Notes field by hand before the import, as a number.
	if _, err := taskField.Create(ctx, project, taskField.Input{Name: "Notes", Type: taskField.TypeNumber}, by, ""); err != nil {
		t.Fatal(err)
	}
	task := func() uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO tasks (id) VALUES ($1)`, id)
		return id
	}
	fieldsOf := func(p uuid.UUID) map[string]*taskFieldModel.Field {
		t.Helper()
		list, err := taskField.List(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*taskFieldModel.Field{}
		for _, f := range list {
			out[f.Name] = f
		}
		return out
	}

	channel := importProvider.SourceField{SourceID: "f1", Name: "Channel", Type: importProvider.FieldSelect, Options: []importProvider.SourceOption{
		{SourceID: "o1", Label: "Blog", Color: "violet"}, {SourceID: "o2", Label: "Email"}, {SourceID: "o3", Label: " blog "},
	}}
	tags := importProvider.SourceField{SourceID: "f2", Name: "Tags", Type: importProvider.FieldMultiSelect, Options: []importProvider.SourceOption{{SourceID: "t1", Label: "Web"}}}
	budget := importProvider.SourceField{SourceID: "f3", Name: "Budget", Type: importProvider.FieldMoney, Currency: "EUR"}
	reviewer := importProvider.SourceField{SourceID: "f4", Name: "Reviewer", Type: importProvider.FieldPerson}
	notes := importProvider.SourceField{SourceID: "f5", Name: "Notes", Type: importProvider.FieldText}
	link := importProvider.SourceField{SourceID: "f6", Name: "Link", Type: importProvider.FieldURL}
	defs := []importProvider.SourceField{channel, tags, budget, reviewer, notes, link}

	// The project stage makes them all, in order, beside the one already there.
	newProjectFields(job, nil, "P1", project, by).ensure(ctx, defs)
	got := fieldsOf(project)
	if len(got) != 7 || got["Notes (2)"] == nil || got["Notes (2)"].Type != taskField.TypeText || got["Notes"].Type != taskField.TypeNumber {
		t.Fatalf("six fields beside the hand-made Notes, the text one renamed: %v", keys(got))
	}
	if opts := got["Channel"].Options; len(opts) != 2 || opts[0].Label != "Blog" || opts[0].Color != "violet" || opts[1].Label != "Email" {
		t.Errorf("options in order, without the namesake: %+v", opts)
	}
	if got["Budget"].Currency != "EUR" {
		t.Errorf("money keeps its currency: %+v", got["Budget"])
	}

	// A later chunk finds the same fields, and learns of an option and a field
	// from values alone.
	t1 := task()
	seen := tags
	seen.Options = append([]importProvider.SourceOption{}, tags.Options...)
	seen.Options = append(seen.Options, importProvider.SourceOption{SourceID: "t2", Label: "App"})
	score := importProvider.SourceField{SourceID: "f7", Name: "Score", Type: importProvider.FieldNumber}
	later := newProjectFields(job, nil, "P1", project, by)
	ready, lost := later.prepare(ctx, []importProvider.SourceFieldValue{
		{Field: channel, Value: "o2"},
		{Field: seen, Value: []string{"t1", "t2"}},
		{Field: budget, Value: 12.5},
		{Field: reviewer, Value: "u-maya"},
		{Field: notes, Value: strings.Repeat("n", taskField.MaxTextLength+100)},
		{Field: link, Value: "example.com/brief"},
		{Field: score, Value: 7.0},
	})
	if len(lost) != 0 || len(ready) != 7 {
		t.Fatalf("every value kept: %d ready, lost %v", len(ready), lost)
	}
	later.write(ctx, t1, ready)
	got = fieldsOf(project)
	if len(got) != 8 || got["Score"] == nil {
		t.Fatalf("one new field, no second copies: %v", keys(got))
	}
	if opts := got["Tags"].Options; len(opts) != 2 || opts[1].Label != "App" {
		t.Errorf("the option first seen on a task is added: %+v", opts)
	}
	values, err := taskFieldModel.ValuesFor(ctx, []uuid.UUID{t1})
	if err != nil {
		t.Fatal(err)
	}
	v := values[t1.String()]
	want := map[string]string{
		"Channel":   `"` + got["Channel"].Options[1].ID + `"`,
		"Tags":      `["` + got["Tags"].Options[0].ID + `","` + got["Tags"].Options[1].ID + `"]`,
		"Budget":    `1250`,
		"Reviewer":  `"` + maya.String() + `"`,
		"Notes (2)": `"` + strings.Repeat("n", taskField.MaxTextLength) + `"`,
		"Link":      `"https://example.com/brief"`,
		"Score":     `7`,
	}
	for name, w := range want {
		if g := compact(v[got[name].ID.String()]); g != w {
			t.Errorf("%s = %.80s, want %.80s", name, g, w)
		}
	}

	// Values that can't be kept go to the task's description instead, said
	// once a field.
	t2 := task()
	ready, lost = later.prepare(ctx, []importProvider.SourceFieldValue{
		{Field: reviewer, Value: "u-nobody", Text: "Nobody Known"},
		{Field: channel, Value: 3.0},
		{Field: budget, Value: "lots"},
		{Field: link, Value: "TBD"},
	})
	later.write(ctx, t2, ready)
	if values, _ := taskFieldModel.ValuesFor(ctx, []uuid.UUID{t2}); len(values[t2.String()]) != 0 {
		t.Errorf("nothing set: %v", values)
	}
	if fmt.Sprint(lost) != "[{Reviewer Nobody Known} {Channel 3} {Budget lots} {Link TBD}]" {
		t.Errorf("each kept for the description: %v", lost)
	}
	var warnings int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_errors WHERE import_id = $1 AND code = 'FIELD_VALUE_SKIPPED'`, job.Id).Scan(&warnings); err != nil {
		t.Fatal(err)
	}
	if warnings != 4 {
		t.Errorf("one warning for each field: %d", warnings)
	}
	// A second chunk meeting the same problems says nothing more.
	newProjectFields(job, nil, "P1", project, by).prepare(ctx, []importProvider.SourceFieldValue{{Field: budget, Value: "lots"}})
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_errors WHERE import_id = $1 AND code = 'FIELD_VALUE_SKIPPED'`, job.Id).Scan(&warnings); err != nil || warnings != 4 {
		t.Errorf("once an import, not once a chunk: %d %v", warnings, err)
	}

	// Two of the source's fields with one name stay two fields.
	twin := importProvider.SourceField{SourceID: "f8", Name: "Channel", Type: importProvider.FieldSelect}
	if f := later.field(ctx, twin); f == nil || f.Name != "Channel (2)" {
		t.Errorf("a second Channel: %+v", f)
	}

	// The next import of the workspace finds the fields the first made, even
	// one it had to rename.
	if err := importModels.UpdateStatus(ctx, job.Id, importModels.StatusCompleted, nil, nil); err != nil {
		t.Fatal(err)
	}
	again := &importModels.Job{Id: uuid.New(), Provider: job.Provider, SourceWorkspaceName: job.SourceWorkspaceName, Source: "api", Status: importModels.StatusRunning}
	if err := importModels.CreateJob(ctx, again); err != nil {
		t.Fatal(err)
	}
	before := len(fieldsOf(project))
	if f := newProjectFields(again, nil, "P1", project, by).field(ctx, notes); f == nil || f.Name != "Notes (2)" || len(fieldsOf(project)) != before {
		t.Errorf("found again, not made a third time: %+v", f)
	}

	// A project at its limit of fields keeps a new field's values as text.
	full := uuid.New()
	for i := 0; i < taskField.MaxPerProject; i++ {
		if _, err := taskField.Create(ctx, full, taskField.Input{Name: fmt.Sprintf("Field %d", i), Type: taskField.TypeText}, by, ""); err != nil {
			t.Fatal(err)
		}
	}
	overflow := importProvider.SourceField{SourceID: "f9", Name: "Overflow", Type: importProvider.FieldText}
	if ready, lost := newProjectFields(job, nil, "P3", full, by).prepare(ctx, []importProvider.SourceFieldValue{{Field: overflow, Value: "hello"}}); len(ready) != 0 || fmt.Sprint(lost) != "[{Overflow hello}]" {
		t.Errorf("past the limit: %v %v", ready, lost)
	}

	// Two chunks making one project's fields at once make each once.
	project2 := uuid.New()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			newProjectFields(job, nil, "P2", project2, by).ensure(ctx, defs)
		}()
	}
	wg.Wait()
	if got := fieldsOf(project2); len(got) != len(defs) {
		t.Errorf("each field once: %v", keys(got))
	}

	// A rollback, once the import's tasks are gone, takes away the fields it
	// made, except one someone has started using; the hand-made field stays.
	exec(`UPDATE tasks SET deleted_at = NOW() WHERE id IN ($1, $2)`, t1, t2)
	used := task()
	if err := taskFieldModel.SetValue(ctx, used, got["Channel"].ID, json.RawMessage(`"`+got["Channel"].Options[0].ID+`"`), by); err != nil {
		t.Fatal(err)
	}
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteUnusedFields(ctx, tx, job.Id); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if after := fieldsOf(project); len(after) != 2 || after["Channel"] == nil || after["Notes"] == nil {
		t.Errorf("Channel (in use) and the hand-made Notes stay: %v", keys(after))
	}
	if after := fieldsOf(project2); len(after) != 0 {
		t.Errorf("the second project's fields go: %v", keys(after))
	}
}

func keys(m map[string]*taskFieldModel.Field) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func compact(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
