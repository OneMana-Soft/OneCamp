package business

// A table's own forms (tables B5; Airtable's form view, Notion's database
// forms): made from the table, living with it, with no project. Each answer
// adds a row to the table, as a project's table form does (row.go): the same
// questions, pages, branches and checks, the same public link (/f/<token>).
//
// Permission model: a table's forms are part of the table's set-up, so only
// someone who can manage the table (its maker or a workspace admin, Table
// Access's Manage) lists, makes, changes or deletes them; their links write
// rows as the form's owner. A form stays open only while its owner can still
// manage its table (load, through tableFormOpen): an owner who loses the
// table closes their forms, and whoever saves the form next owns it.

import (
	"context"
	"encoding/json"
	"errors"

	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	"github.com/google/uuid"
)

// ErrTableForbidden is someone who can't manage the table; ErrTableNotFound
// a table that isn't there (or is in the trash). The controller answers 403
// and 404.
var (
	ErrTableForbidden = errors.New("only the table's managers can change its forms")
	ErrTableNotFound  = errors.New("table not found")
)

// Seams for tests: a table's own forms as stored (a new one is made through
// form.go's createForm, a project's form's seam too).
var (
	tableForms      = formModel.ListOfTable
	updateTableForm = formModel.UpdateOfTable
	deleteTableForm = formModel.DeleteOfTable
	tableFormOf     = formModel.GetOfTable
)

// mayManage says whether who may manage table: ErrTableNotFound for a table
// that isn't there, ErrTableForbidden for one they can't manage.
func mayManage(ctx context.Context, who tableAccess.Who, table uuid.UUID) error {
	d, err := tableDecision(ctx, who, table)
	if errors.Is(err, tableAccess.ErrNotFound) {
		return ErrTableNotFound
	}
	if err != nil {
		return err
	}
	if !d.View {
		return ErrTableNotFound
	}
	if !d.Manage {
		return ErrTableForbidden
	}
	return nil
}

// TableForms is a table's own forms, for someone who can manage it.
func TableForms(ctx context.Context, table, by uuid.UUID) ([]formModel.Form, error) {
	who, err := whoIs(ctx, by)
	if err != nil {
		return nil, err
	}
	if err := mayManage(ctx, who, table); err != nil {
		return nil, err
	}
	return tableForms(ctx, table)
}

// SaveTableForm makes or changes one of a table's own forms. Its answers
// always go to that table, whatever the input names, and it fills only the
// table's columns: no task, priority or assignee.
func SaveTableForm(ctx context.Context, table uuid.UUID, in Input, by uuid.UUID) (*formModel.Form, error) {
	who, err := whoIs(ctx, by)
	if err != nil {
		return nil, err
	}
	if err := mayManage(ctx, who, table); err != nil {
		return nil, err
	}
	in.TargetTable = table.String()
	in.Priority, in.AssigneeUUID, in.TitleField = "", "", ""
	f, err := CheckForm(in)
	if err != nil {
		return nil, err
	}
	var fields []Field
	if err := json.Unmarshal(f.Fields, &fields); err != nil {
		return nil, err
	}
	if err := checkTable(ctx, table, fields, who); err != nil {
		return nil, err
	}
	f.Fields, _ = json.Marshal(fields)
	var was *formModel.Form
	if f.Id != uuid.Nil && (f.CoverID != nil || f.LogoID != nil) {
		if was, err = formModel.GetOfTable(ctx, table, f.Id); err != nil {
			return nil, err
		}
	}
	if err := checkLook(ctx, f, was, by); err != nil {
		return nil, err
	}
	f.ProjectUUID = uuid.Nil
	// Whoever saves a form owns it from then on: its rows are added as them.
	f.CreatedBy = by
	if f.Id == uuid.Nil {
		return newForm(ctx, f, "table")
	}
	saved, err := updateTableForm(ctx, table, f)
	if err == nil && saved == nil {
		return nil, &FormError{"That isn't one of this table's forms."}
	}
	return saved, err
}

// DeleteTableForm deletes one of a table's own forms; false when the table
// has no such form. The rows its answers added stay.
func DeleteTableForm(ctx context.Context, table, id, by uuid.UUID) (bool, error) {
	if err := MayManageTable(ctx, table, by); err != nil {
		return false, err
	}
	return deleteTableForm(ctx, table, id, by)
}

// MayManageTable is the check a table's form's delete and restore both make:
// may by manage table? ErrTableNotFound, ErrTableForbidden.
func MayManageTable(ctx context.Context, table, by uuid.UUID) error {
	who, err := whoIs(ctx, by)
	if err != nil {
		return err
	}
	return mayManage(ctx, who, table)
}

// tableFormOpen says whether a table's own form may take answers: its owner
// can still manage its table.
func tableFormOpen(ctx context.Context, l *live) bool {
	if l.form.TargetTable == nil {
		return false
	}
	d, err := tableDecision(ctx, ownerWho(l), *l.form.TargetTable)
	return err == nil && d.Manage && d.Edit
}

// TableForm is one of a table's own forms, for someone who can manage the
// table; nil when the table has no such form.
func TableForm(ctx context.Context, table, id, by uuid.UUID) (*formModel.Form, error) {
	who, err := whoIs(ctx, by)
	if err != nil {
		return nil, err
	}
	if err := mayManage(ctx, who, table); err != nil {
		return nil, err
	}
	return tableFormOf(ctx, table, id)
}
