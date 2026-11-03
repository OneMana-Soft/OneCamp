package business

// A step over a view's rows (G2a), without a database: the view is a list of
// windows, and the second phase reads and writes through fakes of its seams
// (transformRead, transformBatch).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func stepText(name string) *model.Field {
	return &model.Field{Id: uuid.New(), Name: name, Type: model.FieldText}
}

func stepNumber(name string) *model.Field {
	return &model.Field{Id: uuid.New(), Name: name, Type: model.FieldNumber}
}

// stepRow is a row holding values by field, at version.
func stepRow(version int64, values map[*model.Field]any) *model.Row {
	m := map[string]any{}
	for f, v := range values {
		m[f.Id.String()] = v
	}
	b, _ := json.Marshal(m)
	return &model.Row{Id: uuid.New(), Values: string(b), Version: version}
}

// stepWindows serves rows as a view's walk reads them, n to a window.
func stepWindows(rows []*model.Row, n int) rowWindows {
	return func(context.Context) ([]*model.Row, bool, error) {
		w := rows[:min(n, len(rows))]
		rows = rows[len(w):]
		return w, len(rows) > 0, nil
	}
}

func stepIDs(fields ...*model.Field) []uuid.UUID {
	out := make([]uuid.UUID, len(fields))
	for i, f := range fields {
		out[i] = f.Id
	}
	return out
}

// stepFake stands in for the second phase's reads and writes: the rows as
// they are now, by id, and every batch sent, which answer takes in turn
// (nil: written, with a new change set).
type stepFake struct {
	now     map[uuid.UUID]*model.Row
	sent    []BatchInput
	reads   int
	answers []func(in BatchInput) error
}

func withStepSeams(t *testing.T, rows ...*model.Row) *stepFake {
	f := &stepFake{now: map[uuid.UUID]*model.Row{}}
	for _, r := range rows {
		f.now[r.Id] = r
	}
	read, batch := transformRead, transformBatch
	t.Cleanup(func() { transformRead, transformBatch = read, batch })
	transformRead = func(_ context.Context, _ uuid.UUID, _ Actor, ids []uuid.UUID) ([]*model.Row, error) {
		f.reads++
		var out []*model.Row
		for _, id := range ids {
			if r := f.now[id]; r != nil {
				out = append(out, r)
			}
		}
		return out, nil
	}
	transformBatch = func(_ context.Context, _ uuid.UUID, in BatchInput, _ Actor) (*BatchResult, error) {
		// As sent: the writer may reuse its slices once the batch answers.
		in.Patch, in.Delete = slices.Clone(in.Patch), slices.Clone(in.Delete)
		f.sent = append(f.sent, in)
		if n := len(f.sent); n <= len(f.answers) && f.answers[n-1] != nil {
			if err := f.answers[n-1](in); err != nil {
				return nil, err
			}
		}
		set := uuid.New()
		return &BatchResult{ChangeSet: &set}, nil
	}
	return f
}

func TestAStepIsAskedForInWords(t *testing.T) {
	name, city, amount := stepText("Name"), stepText("City"), stepNumber("Amount")
	all := []*model.Field{name, city, amount}
	for _, c := range []struct {
		why  string
		in   TransformInput
		want string
	}{
		{"a step it doesn't know", TransformInput{Step: "sort", Fields: stepIDs(name)}, "a step is find"},
		{"a replace in a number", TransformInput{Step: StepReplace, Fields: stepIDs(name, amount), Find: "1"}, `"Amount" isn't text`},
		{"a find of nothing", TransformInput{Step: StepFind, Fields: stepIDs(name)}, "type what to find"},
		{"a trim of a field the table hasn't", TransformInput{Step: StepTrim, Fields: []uuid.UUID{uuid.New()}}, "name the fields"},
		{"a split of two fields", TransformInput{Step: StepSplit, Fields: stepIDs(name, city), Separator: ",", Into: stepIDs(amount)}, "split one text field"},
		{"a split without a separator", TransformInput{Step: StepSplit, Fields: stepIDs(name), Into: stepIDs(city)}, "say what the text splits at"},
		{"a split into the field it splits", TransformInput{Step: StepSplit, Fields: stepIDs(name), Separator: ",", Into: stepIDs(city, name)}, `"Name" is the field being split`},
		{"a split with nowhere to put its parts", TransformInput{Step: StepSplit, Fields: stepIDs(name), Separator: ","}, "name the fields the parts go to"},
		{"a split into a number", TransformInput{Step: StepSplit, Fields: stepIDs(name), Separator: ",", Into: stepIDs(amount)}, `"Amount" isn't text`},
	} {
		if _, err := prepareTransform(all, c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.why, err, c.want)
		}
	}
	// What may run: a find in a number, a dedupe over any field, and a
	// split's dry run before its fields are named (it says how many parts).
	for _, in := range []TransformInput{
		{Step: StepFind, Fields: stepIDs(name, amount), Find: "12"},
		{Step: StepDedupe, Fields: stepIDs(name, amount)},
		{Step: StepSplit, Fields: stepIDs(name), Separator: ",", DryRun: true},
	} {
		if _, err := prepareTransform(all, in); err != nil {
			t.Errorf("%s over %d fields: %v", in.Step, len(in.Fields), err)
		}
	}
}

func TestAFindCountsEveryMatchAndListsThemFromAPlace(t *testing.T) {
	name, amount := stepText("Name"), stepNumber("Amount")
	all := []*model.Field{name, amount}
	var rows []*model.Row
	for i := 0; i < 1500; i++ {
		label := fmt.Sprintf("Vendor %d", i)
		if i%2 == 0 {
			label = fmt.Sprintf("Acme %d", i)
		}
		rows = append(rows, stepRow(1, map[*model.Field]any{name: label, amount: float64(i)}))
	}
	find := func(text string, from int) *TransformResult {
		t.Helper()
		tr, err := prepareTransform(all, TransformInput{Step: StepFind, Fields: stepIDs(name, amount), Find: text})
		if err != nil {
			t.Fatalf("find %q: %v", text, err)
		}
		w, err := tr.walk(context.Background(), stepWindows(rows, 500), from)
		if err != nil {
			t.Fatalf("find %q: %v", text, err)
		}
		return w.res
	}
	res := find("acme", 0)
	if res.Walked != 1500 || res.Rows != 750 || res.Cells != 750 || len(res.Matches) != 750 || res.Capped {
		t.Fatalf("acme: walked %d, %d rows, %d cells, %d listed (capped %v), want 1500, 750, 750, 750", res.Walked, res.Rows, res.Cells, len(res.Matches), res.Capped)
	}
	if m := res.Matches[1]; m.Row != rows[2].Id || m.Field != name.Id.String() || m.At != 2 {
		t.Errorf("the second match is %+v, want row 2's name at 2", m)
	}
	// A number reads as it's written: row 1499 holds it twice, its name first.
	res = find("1499", 0)
	if res.Rows != 1 || res.Cells != 2 || len(res.Matches) != 2 || res.Matches[0].Field != name.Id.String() || res.Matches[1].Field != amount.Id.String() || res.Matches[1].At != 1499 {
		t.Errorf("1499: %d rows, %d cells, %+v", res.Rows, res.Cells, res.Matches)
	}
	// Listed from a place on, counted in full.
	res = find("vendor", 1000)
	if res.Rows != 750 || len(res.Matches) != 250 || res.Matches[0].At != 1001 {
		t.Errorf("vendor from 1000: %d rows, %d listed, first at %d; want 750, 250, 1001", res.Rows, len(res.Matches), res.Matches[0].At)
	}
	// Every name holds an e: 1,500 matches, the first 1,000 listed.
	if res = find("e", 0); res.Cells != 1500 || len(res.Matches) != maxMatches || !res.Capped {
		t.Errorf("e: %d cells, %d listed (capped %v), want 1500, %d, capped", res.Cells, len(res.Matches), res.Capped, maxMatches)
	}
}

func TestADedupeKeepsTheFirstRowInViewOrder(t *testing.T) {
	name, city := stepText("Name"), stepText("City")
	rows := []*model.Row{
		stepRow(1, map[*model.Field]any{name: "Acme", city: "Pune"}),
		stepRow(1, map[*model.Field]any{name: "Acme", city: "Delhi"}),
		stepRow(1, map[*model.Field]any{name: "Acme", city: "Pune"}), // repeats 0
		stepRow(1, map[*model.Field]any{name: "acme", city: "Pune"}), // case counts
		stepRow(1, map[*model.Field]any{name: "Acme", city: ""}),
		stepRow(1, map[*model.Field]any{name: "Acme"}), // empty and missing are equal: repeats 4
	}
	tr, err := prepareTransform([]*model.Field{name, city}, TransformInput{Step: StepDedupe, Fields: stepIDs(name, city), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	w, err := tr.walk(context.Background(), stepWindows(rows, 2), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []duplicate{{row: rows[2].Id, of: rows[0].Id}, {row: rows[5].Id, of: rows[4].Id}}
	if fmt.Sprint(w.dups) != fmt.Sprint(want) || w.res.Rows != 2 {
		t.Fatalf("duplicates %v (%d rows), want rows 2 and 5, of 0 and 4", w.dups, w.res.Rows)
	}
	if m := w.res.Matches; len(m) != 2 || m[0].At != 2 || m[1].At != 5 || m[0].Field != "" {
		t.Errorf("listed %+v, want rows at 2 and 5", m)
	}
}

func TestAViewPastTheCapIsRefusedBeforeAnyWrite(t *testing.T) {
	name := stepText("Name")
	f := withStepSeams(t)
	row := stepRow(1, map[*model.Field]any{name: " Acme "})
	rows := make([]*model.Row, maxTransformRows+1)
	for i := range rows {
		rows[i] = row
	}
	tr, err := prepareTransform([]*model.Field{name}, TransformInput{Step: StepTrim, Fields: stepIDs(name)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.walk(context.Background(), stepWindows(rows, 500), 0); !errors.Is(err, errTransformRows) {
		t.Errorf("a view of %d rows: %v, want %v", len(rows), err, errTransformRows)
	}
	if len(f.sent) != 0 {
		t.Errorf("%d batches sent", len(f.sent))
	}
}

// stepRun walks rows and writes what the step changes, through the fakes.
func stepRun(t *testing.T, all []*model.Field, in TransformInput, rows []*model.Row) *TransformResult {
	t.Helper()
	tr, err := prepareTransform(all, in)
	if err != nil {
		t.Fatal(err)
	}
	w, err := tr.walk(context.Background(), stepWindows(rows, 500), 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.write(context.Background(), uuid.New(), Actor{UserID: uuid.New()}, tr)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAStepWritesChunksOfOneOperation(t *testing.T) {
	name := stepText("Name")
	var rows []*model.Row
	for i := 0; i < 2500; i++ {
		rows = append(rows, stepRow(int64(i+1), map[*model.Field]any{name: fmt.Sprintf("Acme %d", i)}))
	}
	f := withStepSeams(t, rows...)
	res := stepRun(t, []*model.Field{name}, TransformInput{Step: StepReplace, Fields: stepIDs(name), Find: "acme", Replace: "ACME"}, rows)
	if len(f.sent) != 3 || len(f.sent[0].Patch) != 1000 || len(f.sent[1].Patch) != 1000 || len(f.sent[2].Patch) != 500 {
		t.Fatalf("%d batches sent, want 3 of 1,000, 1,000 and 500 rows", len(f.sent))
	}
	for i, in := range f.sent {
		if in.Operation == uuid.Nil || in.Operation != f.sent[0].Operation || in.Kind != "replace" {
			t.Errorf("batch %d: operation %s, kind %q; want the first's, replace", i, in.Operation, in.Kind)
		}
	}
	if p := f.sent[1].Patch[0]; p.Row != rows[1000].Id || p.Version != 1001 || p.Values[name.Id.String()] != "ACME 1000" {
		t.Errorf("batch 1's first patch is %+v, want row 1000 at version 1001 as ACME 1000", p)
	}
	if res.Rows != 2500 || res.Cells != 2500 || len(res.ChangeSets) != 3 || res.Operation == nil || *res.Operation != f.sent[0].Operation {
		t.Errorf("answered %d rows, %d cells, %d change sets, operation %v", res.Rows, res.Cells, len(res.ChangeSets), res.Operation)
	}
}

func TestARowWrittenAgainMidStepIsLeftAsItIs(t *testing.T) {
	name := stepText("Name")
	rows := []*model.Row{
		stepRow(1, map[*model.Field]any{name: "  Acme  "}),
		stepRow(1, map[*model.Field]any{name: "  Bolt  "}),
		stepRow(1, map[*model.Field]any{name: "  Cove  "}),
	}
	f := withStepSeams(t, rows...)
	edit := func(version int64, text string) func(BatchInput) error {
		return func(BatchInput) error {
			r := stepRow(version, map[*model.Field]any{name: text})
			r.Id = rows[1].Id
			f.now[r.Id] = r
			return &RowConflict{Row: r}
		}
	}
	// Someone writes row 1 between each read and the write after it, twice.
	f.answers = []func(BatchInput) error{edit(2, "  Bolt Ltd  "), edit(3, "Bolt plc")}
	res := stepRun(t, []*model.Field{name}, TransformInput{Step: StepTrim, Fields: stepIDs(name)}, rows)
	if len(f.sent) != 3 || f.reads != 2 {
		t.Fatalf("%d batches sent after %d reads, want 3 after 2", len(f.sent), f.reads)
	}
	if p := f.sent[1].Patch[1]; p.Row != rows[1].Id || p.Version != 2 || p.Values[name.Id.String()] != "Bolt Ltd" {
		t.Errorf("after the first conflict, row 1's patch is %+v, want version 2 as Bolt Ltd", p)
	}
	if last := f.sent[2].Patch; len(last) != 2 || last[0].Row != rows[0].Id || last[1].Row != rows[2].Id {
		t.Errorf("the last batch writes %d rows, want rows 0 and 2", len(last))
	}
	if res.KeptChanged != 1 || res.Rows != 2 || len(res.ChangeSets) != 1 {
		t.Errorf("kept %d, wrote %d rows in %d change sets; want 1, 2, 1", res.KeptChanged, res.Rows, len(res.ChangeSets))
	}
}

func TestCellsTheServerRefusesAreSkippedAndTheRestWritten(t *testing.T) {
	email := &model.Field{Id: uuid.New(), Name: "Email", Type: model.FieldEmail}
	rows := []*model.Row{
		stepRow(1, map[*model.Field]any{email: "ann@acme.test"}),
		stepRow(1, map[*model.Field]any{email: "bo@acme.test"}),
	}
	f := withStepSeams(t, rows...)
	f.answers = []func(BatchInput) error{func(BatchInput) error {
		return BatchErrors{{Row: rows[1].Id, Field: email.Id.String(), Msg: "isn't an email address"}}
	}}
	res := stepRun(t, []*model.Field{email}, TransformInput{Step: StepReplace, Fields: stepIDs(email), Find: "@acme.test", Replace: " at acme"}, rows)
	if len(f.sent) != 2 || len(f.sent[1].Patch) != 1 || f.sent[1].Patch[0].Row != rows[0].Id {
		t.Fatalf("%d batches sent, want the second with row 0 alone", len(f.sent))
	}
	if res.Rows != 1 || res.SkippedCount != 1 || len(res.Skipped) != 1 || res.Skipped[0].Row != rows[1].Id || res.Skipped[0].Msg != "isn't an email address" {
		t.Errorf("wrote %d rows, skipped %d: %+v", res.Rows, res.SkippedCount, res.Skipped)
	}
}

func TestADedupeDeletesOnlyRowsStillEqualToTheRowTheyRepeat(t *testing.T) {
	name := stepText("Name")
	rows := []*model.Row{
		stepRow(1, map[*model.Field]any{name: "Acme"}),
		stepRow(1, map[*model.Field]any{name: "Acme"}), // repeats 0: deleted
		stepRow(1, map[*model.Field]any{name: "Bolt"}),
		stepRow(1, map[*model.Field]any{name: "Bolt"}), // repeats 2, which changes: kept
		stepRow(1, map[*model.Field]any{name: "Acme"}), // repeats 0, deleted by someone meanwhile
	}
	f := withStepSeams(t, rows...)
	tr, err := prepareTransform([]*model.Field{name}, TransformInput{Step: StepDedupe, Fields: stepIDs(name)})
	if err != nil {
		t.Fatal(err)
	}
	w, err := tr.walk(context.Background(), stepWindows(rows, 500), 0)
	if err != nil {
		t.Fatal(err)
	}
	changed := stepRow(2, map[*model.Field]any{name: "Bolt Ltd"})
	changed.Id = rows[2].Id
	f.now[changed.Id] = changed
	delete(f.now, rows[4].Id)
	res, err := w.write(context.Background(), uuid.New(), Actor{UserID: uuid.New()}, tr)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || fmt.Sprint(f.sent[0].Delete) != fmt.Sprint([]uuid.UUID{rows[1].Id}) || f.sent[0].Kind != "dedupe" || f.sent[0].Operation == uuid.Nil {
		t.Fatalf("sent %+v, want one dedupe deleting row 1", f.sent)
	}
	if res.Deleted != 1 || res.KeptChanged != 1 || len(res.ChangeSets) != 1 {
		t.Errorf("deleted %d, kept %d, %d change sets; want 1, 1, 1", res.Deleted, res.KeptChanged, len(res.ChangeSets))
	}
}
