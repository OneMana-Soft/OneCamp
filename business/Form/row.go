package business

// A form that adds a row to a data table instead of making a task (Notion's
// and Airtable's forms; tables B5's row target). Each question fills one of
// the table's columns, by kind, through the tables' one writer (RowLedger
// Rows) as the form's owner, marked a form's: so the table's rules decide
// what each cell may hold, and its required columns are enforced. The owner
// must be able to add rows to the table, when the form is saved and every
// time it is opened or sent.
//
// A visitor's words never act with the owner's reach: a question fills only
// a column that holds what was typed (words, a number, a date, a tick, a
// choice), never a link to other rows or items, a person, or a worked-out
// column, and the row is written marked as coming from outside
// (helpers.WithPublicSubmission), so an agent the new row wakes takes it as
// nobody's request.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	tableModel "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// MapColumn + a data table field's id fills that column of the form's table.
const MapColumn = "column:"

// Seams for tests: the table's columns, what someone may do with it, and the
// tables' one writer.
var (
	tableColumns  = tableModel.ListFields
	tableDecision = func(ctx context.Context, who tableAccess.Who, table uuid.UUID) (tableAccess.Decision, error) {
		return tableAccess.Tables.Resolve(ctx, who, table)
	}
	writeRows = func(ctx context.Context, by rowLedger.By, table uuid.UUID, rows []rowLedger.RowInput, at rowLedger.Position) ([]*tableModel.Row, error) {
		return rowLedger.Rows.Create(ctx, by, table, rows, at)
	}
	// whoIs is someone as a table decides for them: their id and whether
	// they're a workspace admin.
	whoIs = func(ctx context.Context, id uuid.UUID) (tableAccess.Who, error) {
		u, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
		if err != nil {
			return tableAccess.Who{}, err
		}
		if u == nil {
			return tableAccess.Who{}, &FormError{"Only a member of the workspace can save a form."}
		}
		return tableAccess.Who{User: id, Admin: u.IsAdmin}, nil
	}
)

// columnOf is the column a question fills, or uuid.Nil.
func columnOf(fl Field) uuid.UUID {
	if !strings.HasPrefix(fl.MapsTo, MapColumn) {
		return uuid.Nil
	}
	id, _ := uuid.Parse(strings.TrimPrefix(fl.MapsTo, MapColumn))
	return id
}

// checkColumn says what's wrong with the column question n of a form that
// adds rows fills, as far as the question alone tells, or "": every question
// fills one. Pure.
func checkColumn(fl *Field, n int) string {
	fl.Map = nil
	id := columnOf(*fl)
	if id == uuid.Nil {
		return fmt.Sprintf("Question %d needs the column it fills.", n)
	}
	fl.MapsTo = MapColumn + id.String()
	return ""
}

// checkTable checks a form that adds rows against its table, as whoever saves
// it: they must be able to add rows to it; each question fills one of its
// columns, of a kind the question suits (fills), and a choice takes the
// column's options, so what a visitor picks is always one of them; and every
// column a row must have is filled by a question that is always asked and
// required, unless the column starts with a value of its own. The columns
// are read once.
func checkTable(ctx context.Context, table uuid.UUID, fields []Field, saver tableAccess.Who) error {
	d, err := tableDecision(ctx, saver, table)
	if errors.Is(err, tableAccess.ErrNotFound) || (err == nil && !d.Edit) {
		return &FormError{"Pick a table you can add rows to."}
	}
	if err != nil {
		return err
	}
	cols, err := tableColumns(ctx, table)
	if err != nil {
		return err
	}
	byID := make(map[uuid.UUID]*tableModel.Field, len(cols))
	for _, c := range cols {
		byID[c.Id] = c
	}
	always := alwaysAsked(fields)
	filledAlways := map[uuid.UUID]bool{}
	for i := range fields {
		if fields[i].Type == Section {
			continue
		}
		c := byID[columnOf(fields[i])]
		if c == nil {
			return &FormError{fmt.Sprintf("“%s” fills a column that table doesn't have.", fields[i].Label)}
		}
		if !slices.Contains(fills[c.Type], fields[i].Type) {
			if len(fills[c.Type]) == 0 {
				return &FormError{fmt.Sprintf("A form can't fill %s.", c.Name)}
			}
			return &FormError{fmt.Sprintf("“%s” can't fill %s: ask it as %s.", fields[i].Label, c.Name, kindsFor(c.Type))}
		}
		if c.Type == tableModel.FieldSelect || c.Type == tableModel.FieldMultiSelect {
			opts := fieldTypes.OptionsOf(c)
			if len(opts) < 2 || len(opts) > maxChoices {
				return &FormError{fmt.Sprintf("%s needs 2 to %d options for a form to offer.", c.Name, maxChoices)}
			}
			fields[i].Options = make([]string, len(opts))
			for j, o := range opts {
				fields[i].Options[j] = o.Label
			}
		}
		if always[fields[i].Id] {
			filledAlways[c.Id] = true
		}
	}
	for _, c := range cols {
		k, known := fieldTypes.Kinds.Lookup(c.Type)
		if !c.Required || !known || !k.Stored || filledAlways[c.Id] || startsFilled(c) {
			continue
		}
		if len(fills[c.Type]) == 0 {
			return &FormError{fmt.Sprintf("Every row of the table needs %s, which a form can't fill.", c.Name)}
		}
		return &FormError{fmt.Sprintf("Every row of the table needs %s: add a question for it that is always asked and required.", c.Name)}
	}
	return nil
}

// alwaysAsked is the questions every visitor answers: required, with no
// condition of their own or on the page they're on. Pure.
func alwaysAsked(fields []Field) map[string]bool {
	out := map[string]bool{}
	page := true
	for _, f := range fields {
		if f.Type == Section {
			page = f.ShowIf == nil
			continue
		}
		if page && f.ShowIf == nil && f.Required {
			out[f.Id] = true
		}
	}
	return out
}

// startsFilled says whether a column's new rows start with a value of its
// own (its default). Pure.
func startsFilled(c *tableModel.Field) bool {
	if len(c.Default) == 0 {
		return false
	}
	var v any
	return json.Unmarshal(c.Default, &v) == nil && !fieldTypes.IsEmpty(v)
}

// Cells are the answers as the row's cells, by column id, each as its column
// takes it: words as they were written, a number as it was read, a date as
// its day, a tick as true or false, choices by their labels. An answer whose
// column the table no longer has is left out. Pure.
func Cells(fields []Field, answers []Answer, cols []*tableModel.Field) map[string]any {
	byQ := make(map[string]Field, len(fields))
	for _, f := range fields {
		byQ[f.Id] = f
	}
	byID := make(map[uuid.UUID]*tableModel.Field, len(cols))
	for _, c := range cols {
		byID[c.Id] = c
	}
	cells := map[string]any{}
	for _, a := range answers {
		c := byID[columnOf(byQ[a.Id])]
		if c == nil {
			continue
		}
		switch c.Type {
		case tableModel.FieldNumber:
			if a.Num == nil {
				continue
			}
			cells[c.Id.String()] = *a.Num
		case tableModel.FieldCheckbox:
			cells[c.Id.String()] = a.Value == "Yes"
		case tableModel.FieldMultiSelect:
			labels := a.Values
			if len(labels) == 0 {
				labels = []string{a.Value}
			}
			list := make([]any, len(labels))
			for i, l := range labels {
				list[i] = l
			}
			cells[c.Id.String()] = list
		default:
			cells[c.Id.String()] = a.Value
		}
	}
	return cells
}

// ownerWho is the form's owner as its table decides for them.
func ownerWho(l *live) tableAccess.Who {
	return tableAccess.Who{User: l.form.CreatedBy, Admin: l.owner.UserPostgresInfo.IsAdmin}
}

// rowsOpen says whether a form that adds rows may take answers: its owner can
// still add rows to its table.
func rowsOpen(ctx context.Context, l *live) bool {
	if l.form.TargetTable == nil {
		return false
	}
	d, err := tableDecision(ctx, ownerWho(l), *l.form.TargetTable)
	return err == nil && d.Edit
}

// submitRow adds the submission's row to the form's table, as the form's
// owner, and keeps the submission with the row. A row the table refuses
// (a column made required since, a choice since removed) is nothing the
// visitor can fix: the form says it can't take answers, and the reason is
// logged for the team.
func submitRow(ctx context.Context, l *live, answers []Answer) error {
	table := *l.form.TargetTable
	cols, err := tableColumns(ctx, table)
	if err != nil {
		return err
	}
	by := rowLedger.By{Who: ownerWho(l), Kind: rowLedger.ActorForm}
	rows, err := writeRows(ctx, by, table, []rowLedger.RowInput{{Cells: Cells(l.fields, answers, cols)}}, rowLedger.Position{})
	if err != nil {
		var refused fieldTypes.CellErrors
		if errors.As(err, &refused) {
			helpers.LogWarnWithContext(ctx, "business/Form %s: its table %s refused a row: %v", l.form.Id, table, err)
			return &FormError{"This form can't take answers right now: the table it fills has changed. Try again later."}
		}
		return err
	}
	if len(rows) == 0 || rows[0] == nil {
		return errors.New("forms: the table wrote no row")
	}
	row := rows[0].Id
	stored, _ := json.Marshal(answers)
	if _, err := recordSubmission(ctx, l.form.Id, nil, &row, stored); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Form/Submit record err: %+v", err)
	}
	return nil
}
