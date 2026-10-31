package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	"github.com/akashc777/OneCamp/business/Formula"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	"github.com/akashc777/OneCamp/helpers"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// vendors is a table of vendors: a name every row needs, an email every row
// needs but that starts as the team's inbox, a tier, services, a budget, a
// start date, whether they're signed, a site, a person and a worked-out score.
func vendors() (uuid.UUID, []*tableModel.Field) {
	col := func(name, typ string, required bool, config string) *tableModel.Field {
		if config == "" {
			config = "{}"
		}
		return &tableModel.Field{Id: uuid.New(), Name: name, Type: typ, Required: required, Config: config}
	}
	name := col("Name", tableModel.FieldText, true, "")
	email := col("Email", tableModel.FieldEmail, true, "")
	email.Default = json.RawMessage(`"vendors@example.com"`)
	return uuid.New(), []*tableModel.Field{
		name, email,
		col("Tier", tableModel.FieldSelect, false, `{"options":[{"id":"o_1","label":"Gold"},{"id":"o_2","label":"Silver"}]}`),
		col("Services", tableModel.FieldMultiSelect, false, `{"options":[{"id":"o_3","label":"Print"},{"id":"o_4","label":"Video"},{"id":"o_5","label":"Web"}]}`),
		col("Budget", tableModel.FieldNumber, false, ""),
		col("Starts", tableModel.FieldDate, false, ""),
		col("Signed", tableModel.FieldCheckbox, false, ""),
		col("Site", tableModel.FieldURL, false, ""),
		col("Owner", tableModel.FieldPerson, false, ""),
		col("Score", tableModel.FieldFormula, false, `{"formula":"1"}`),
	}
}

func column(cols []*tableModel.Field, name string) string {
	for _, c := range cols {
		if c.Name == name {
			return MapColumn + c.Id.String()
		}
	}
	return ""
}

// signUp is a vendor sign-up form that fills the vendors table.
func signUp(cols []*tableModel.Field) []Field {
	return []Field{
		{Id: "name", Label: "Company", Type: ShortText, Required: true, MapsTo: column(cols, "Name")},
		{Id: "tier", Label: "Tier", Type: Select, Options: []string{"ignored", "the column's own"}, MapsTo: column(cols, "Tier")},
		{Id: "services", Label: "Services", Type: MultiSelect, Options: []string{"ignored", "the column's own"}, MapsTo: column(cols, "Services")},
		{Id: "budget", Label: "Budget", Type: Number, MapsTo: column(cols, "Budget")},
		{Id: "starts", Label: "Start", Type: Date, MapsTo: column(cols, "Starts")},
		{Id: "signed", Label: "Signed?", Type: Checkbox, MapsTo: column(cols, "Signed")},
		{Id: "site", Label: "Website", Type: URL, MapsTo: column(cols, "Site")},
	}
}

func withTable(t *testing.T, table uuid.UUID, cols []*tableModel.Field, edit bool) *[]tableAccess.Who {
	t.Helper()
	var asked []tableAccess.Who
	saved := struct {
		cols   func(context.Context, uuid.UUID) ([]*tableModel.Field, error)
		access func(context.Context, tableAccess.Who, uuid.UUID) (tableAccess.Decision, error)
	}{tableColumns, tableDecision}
	t.Cleanup(func() { tableColumns, tableDecision = saved.cols, saved.access })
	tableColumns = func(_ context.Context, id uuid.UUID) ([]*tableModel.Field, error) {
		if id != table {
			return nil, nil
		}
		return cols, nil
	}
	tableDecision = func(_ context.Context, who tableAccess.Who, id uuid.UUID) (tableAccess.Decision, error) {
		asked = append(asked, who)
		if id != table {
			return tableAccess.Decision{}, tableAccess.ErrNotFound
		}
		return tableAccess.Decision{View: edit, Edit: edit}, nil
	}
	return &asked
}

// A form can add a row to a data table instead of making a task: every
// question fills one of the table's columns, of a kind its answer suits, two
// questions never fill one column, and a form that adds rows asks for no
// files yet (a table row keeps none).
func TestAFormThatAddsRowsFillsAColumnWithEachQuestion(t *testing.T) {
	table, cols := vendors()
	in := func(fields []Field) Input {
		return Input{Title: "Vendor sign-up", TargetTable: table.String(), Fields: fields, Priority: "high", AssigneeUUID: uuid.NewString()}
	}
	f, err := CheckForm(in(signUp(cols)))
	if err != nil {
		t.Fatalf("a sign-up form: %v", err)
	}
	if f.TargetTable == nil || *f.TargetTable != table || f.AssigneeUUID != nil || f.Priority != "medium" {
		t.Errorf("target %v, assignee %v, priority %q: a row has no assignee or priority", f.TargetTable, f.AssigneeUUID, f.Priority)
	}
	refused := map[string][]Field{
		"a question that fills no column":         append(signUp(cols), Field{Id: "x", Label: "Notes", Type: LongText}),
		"a question that fills a task's priority": append(signUp(cols), Field{Id: "x", Label: "Urgency", Type: Select, Options: []string{"a", "b"}, MapsTo: MapPriority, Map: map[string]string{"a": "high", "b": "low"}}),
		"two questions for one column":            append(signUp(cols), Field{Id: "x", Label: "Also the name", Type: ShortText, MapsTo: column(cols, "Name")}),
		"a file question":                         append(signUp(cols), Field{Id: "x", Label: "Brochure", Type: File}),
	}
	for name, fields := range refused {
		if _, err := CheckForm(in(fields)); err == nil {
			t.Errorf("%s was saved", name)
		}
	}
	// A form that makes tasks fills no column.
	if _, err := CheckForm(Input{Title: "x", Fields: []Field{{Id: "a", Label: "A", Type: ShortText, MapsTo: column(cols, "Name")}}}); err == nil {
		t.Error("a task form filling a column was saved")
	}
}

// Saving a form that adds rows checks it against the table, as whoever saves
// it: they must be able to add rows to it, each column must be the table's
// and of a kind the question suits (never a person, a link or a worked-out
// column), a choice takes the column's options, and every column a row must
// have is filled by a question that is always asked and required, unless the
// column starts with a value of its own.
func TestAFormThatAddsRowsIsCheckedAgainstItsTable(t *testing.T) {
	table, cols := vendors()
	saver := tableAccess.Who{User: uuid.New(), Admin: true}
	asked := withTable(t, table, cols, true)
	fields := signUp(cols)
	if err := checkTable(context.Background(), table, fields, saver); err != nil {
		t.Fatalf("the sign-up form: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0] != saver {
		t.Errorf("access was asked for %v, want the saver", *asked)
	}
	if got := strings.Join(fields[1].Options, ","); got != "Gold,Silver" {
		t.Errorf("the tier question offers %q, not the column's options", got)
	}
	if got := strings.Join(fields[2].Options, ","); got != "Print,Video,Web" {
		t.Errorf("the services question offers %q", got)
	}
	bad := map[string]func([]Field) []Field{
		"a column the table doesn't have": func(f []Field) []Field { f[3].MapsTo = MapColumn + uuid.NewString(); return f },
		"words into a number":             func(f []Field) []Field { f[3].Type = ShortText; return f },
		"a person column":                 func(f []Field) []Field { f[6].MapsTo = column(cols, "Owner"); f[6].Type = ShortText; return f },
		"a worked-out column":             func(f []Field) []Field { f[6].MapsTo = column(cols, "Score"); f[6].Type = ShortText; return f },
		"the name optional":               func(f []Field) []Field { f[0].Required = false; return f },
		"the name shown on an answer": func(f []Field) []Field {
			f[0].ShowIf = &ShowIf{Question: "x", Op: ShowAnswered}
			return append([]Field{{Id: "x", Label: "X", Type: ShortText}}, f...)
		},
		"the name on a page shown on an answer": func(f []Field) []Field {
			return append([]Field{{Id: "x", Label: "X", Type: Checkbox, MapsTo: column(cols, "Signed")},
				{Id: "s", Label: "About you", Type: Section, ShowIf: &ShowIf{Question: "x", Op: ShowIs, Values: []string{"Yes"}}}},
				append(f[:5], f[6:]...)...)
		},
		"no question for the name": func(f []Field) []Field { return f[1:] },
	}
	for name, change := range bad {
		if err := checkTable(context.Background(), table, change(signUp(cols)), saver); err == nil {
			t.Errorf("%s was saved", name)
		}
	}
	// The email column needs a value in every row too, but starts with the
	// team's inbox: no question has to fill it.
	withTable(t, table, cols, false)
	var fe *FormError
	if err := checkTable(context.Background(), table, signUp(cols), saver); !errors.As(err, &fe) {
		t.Errorf("a saver who can't add rows: %v, want a FormError", err)
	}
	if err := checkTable(context.Background(), uuid.New(), signUp(cols), saver); !errors.As(err, &fe) {
		t.Errorf("a table that isn't there: %v, want a FormError", err)
	}
}

// Answers become the row's cells by their columns' kinds: words as words, a
// number as the number it was read as, a date as its day, a tick as true or
// false, choices by their labels. An answer whose column is gone is left out.
func TestAnswersBecomeARowsCells(t *testing.T) {
	_, cols := vendors()
	fields := signUp(cols)
	fields[1].Options = []string{"Gold", "Silver"}
	fields[2].Options = []string{"Print", "Video", "Web"}
	answers, err := CheckAnswers(fields, map[string]any{
		"name": "Acme", "services": []any{"Print", "Web"}, "budget": "1,200.50", "starts": "2026-11-02", "signed": true, "site": "https://acme.test",
	}, formula.NumberSigns{})
	if err != nil {
		t.Fatal(err)
	}
	cells := Cells(fields, append(answers, Answer{Id: "tier", Value: "Gold"}), cols[:9])
	id := func(name string) string { return strings.TrimPrefix(column(cols, name), MapColumn) }
	want := map[string]any{
		id("Name"): "Acme", id("Services"): []any{"Print", "Web"}, id("Budget"): 1200.5, id("Starts"): "2026-11-02",
		id("Signed"): true, id("Site"): "https://acme.test", id("Tier"): "Gold",
	}
	got, _ := json.Marshal(cells)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Errorf("cells %s\nwant  %s", got, exp)
	}
	// Each cell is one its column's kind stores as given.
	if _, err := fieldTypes.Kinds.Check(cols, cells, fieldTypes.Input{New: true, Complete: true}); err != nil {
		t.Errorf("the table refuses the cells: %v", err)
	}
	if len(Cells(fields, answers, nil)) != 0 {
		t.Error("answers filled columns the table no longer has")
	}
}

// Sending a form that adds rows writes the row through the tables' one
// writer, as the form's owner, marked a form's (so its required columns are
// enforced), with the visitor's words marked as coming from outside; the
// submission keeps the row, and no project rule hears of it.
func TestSendingAFormAddsItsRowAsItsOwner(t *testing.T) {
	table, cols := vendors()
	withTable(t, table, cols, true)
	l, _ := visitorForm(t)
	l.fields = signUp(cols)
	l.fields[1].Options = []string{"Gold", "Silver"}
	l.fields[2].Options = []string{"Print", "Video", "Web"}
	l.form.TargetTable = &table
	l.owner.UserPostgresInfo.IsAdmin = true
	row := uuid.New()
	var by rowLedger.By
	var cells map[string]any
	outside := false
	saved := struct {
		write  func(context.Context, rowLedger.By, uuid.UUID, []rowLedger.RowInput, rowLedger.Position) ([]*tableModel.Row, error)
		record func(context.Context, uuid.UUID, *uuid.UUID, *uuid.UUID, []byte) (uuid.UUID, error)
		count  func(context.Context, uuid.UUID, time.Time) (int, error)
		tell   func(context.Context, map[string]interface{})
	}{writeRows, recordSubmission, countSince, tellRules}
	t.Cleanup(func() {
		writeRows, recordSubmission, countSince, tellRules = saved.write, saved.record, saved.count, saved.tell
	})
	countSince = func(context.Context, uuid.UUID, time.Time) (int, error) { return 0, nil }
	writeRows = func(ctx context.Context, b rowLedger.By, id uuid.UUID, rows []rowLedger.RowInput, _ rowLedger.Position) ([]*tableModel.Row, error) {
		by, cells, outside = b, rows[0].Cells, helpers.IsPublicSubmission(ctx)
		if id != table {
			t.Errorf("wrote to %s", id)
		}
		return []*tableModel.Row{{Id: row, TableId: table}}, nil
	}
	var kept *uuid.UUID
	recordSubmission = func(_ context.Context, _ uuid.UUID, task, r *uuid.UUID, _ []byte) (uuid.UUID, error) {
		if task != nil {
			t.Error("a row's submission names a task")
		}
		kept = r
		return uuid.New(), nil
	}
	told := false
	tellRules = func(context.Context, map[string]interface{}) { told = true }

	if err := Submit(context.Background(), l.form.Token, map[string]any{"name": "Acme", "tier": "Gold"}, formula.NumberSigns{}, "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if by.User != l.form.CreatedBy || !by.Admin || by.Kind != rowLedger.ActorForm {
		t.Errorf("written by %+v, want the owner, as an admin, by a form", by)
	}
	if !outside {
		t.Error("the visitor's words weren't marked as coming from outside")
	}
	if cells[strings.TrimPrefix(column(cols, "Name"), MapColumn)] != "Acme" || cells[strings.TrimPrefix(column(cols, "Tier"), MapColumn)] != "Gold" {
		t.Errorf("cells %v", cells)
	}
	if kept == nil || *kept != row {
		t.Errorf("the submission keeps row %v, want %s", kept, row)
	}
	if told {
		t.Error("a project rule heard of a row")
	}

	// The table refusing the row's cells (a column made required since) is
	// nothing the visitor can fix: the form says it can't take answers.
	writeRows = func(context.Context, rowLedger.By, uuid.UUID, []rowLedger.RowInput, rowLedger.Position) ([]*tableModel.Row, error) {
		return nil, fieldTypes.CellErrors{{Field: "x", Msg: `"Phone" is required`}}
	}
	var fe *FormError
	err := Submit(context.Background(), l.form.Token, map[string]any{"name": "Acme"}, formula.NumberSigns{}, "", "", time.Now())
	if !errors.As(err, &fe) || strings.Contains(fe.Error(), "Phone") {
		t.Errorf("a row the table refuses: %v, want a FormError that doesn't name the table's columns", err)
	}
}

// A form that adds rows is open only while its owner can still add rows to
// its table.
func TestAFormThatAddsRowsClosesWhenItsOwnerCantAddRows(t *testing.T) {
	table, cols := vendors()
	l, _ := visitorForm(t)
	l.form.TargetTable = &table
	withTable(t, table, cols, true)
	if !rowsOpen(context.Background(), l) {
		t.Error("closed while its owner can add rows")
	}
	withTable(t, table, cols, false)
	if rowsOpen(context.Background(), l) {
		t.Error("open after its owner lost the table")
	}
}
