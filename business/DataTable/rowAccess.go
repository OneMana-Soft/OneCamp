package business

// The Access Resolver's "table_row" (tableAccess.KindTableRow): a row is
// decided by its table, by today's rule (tableKind, day0.go), until Table
// Access (#66, A5) adds rules of its own for rows; a row in the trash is open
// to nobody outside it, with Was saying what they had. Threads on a row
// (business/TableComments) and notices about one ask it by the row's id alone,
// and that id is never trusted beyond naming a row: the row's own table
// decides, never a table the asker names.
//
// RowTitles names rows for a caller that has decided first who may open them
// (a notice, the inbox): a row as its links name it, and its table's name.

import (
	"context"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	formula "github.com/akashc777/OneCamp/business/Formula"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func init() { accessBusiness.RegisterKind(tableAccess.KindTableRow, rowKind{}) }

// Seams for tests: where rows' tables, tables, fields and cells are read.
var (
	rowHomes    = model.RowHomes
	tablesByIDs = model.TablesByIDs
	fieldsOf    = model.ListFields
	cellsOf     = model.CellsOf
)

// rowKind is the Access Resolver's "table_row".
type rowKind struct{}

func (rowKind) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	asked := make(map[uuid.UUID]string, len(ids))
	wanted := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if parsed, err := uuid.Parse(id); err == nil {
			if _, dup := asked[parsed]; !dup {
				wanted = append(wanted, parsed)
			}
			asked[parsed] = id
		}
	}
	homes, err := rowHomes(ctx, wanted)
	if err != nil {
		return nil, err
	}
	tableIDs := make([]uuid.UUID, 0, len(homes))
	seen := map[uuid.UUID]bool{}
	for _, h := range homes {
		if !seen[h.Table] {
			seen[h.Table] = true
			tableIDs = append(tableIDs, h.Table)
		}
	}
	tables, err := tablesByIDs(ctx, tableIDs)
	if err != nil {
		return nil, err
	}
	user, _ := uuid.Parse(who.UserUUID)
	a := Actor{UserID: user, IsAdmin: who.Admin}
	byTable := make(map[uuid.UUID]accessBusiness.Decision, len(tables))
	for _, t := range tables {
		byTable[t.Id] = tableDecision(t, a)
	}
	out := make(map[string]accessBusiness.Decision, len(homes))
	for id, h := range homes {
		d, ok := byTable[h.Table]
		if !ok {
			continue // its table isn't there: no row either
		}
		out[asked[id]] = rowDecision(d, h.Deleted)
	}
	return out, nil
}

// rowDecision is a row's decision from its table's: the table's, unless the
// row is in the trash, which is open to nobody outside it. Pure.
func rowDecision(table accessBusiness.Decision, deleted bool) accessBusiness.Decision {
	if !deleted || table.Deleted {
		return table
	}
	d := table
	d.Was, d.Level, d.Unlocked, d.Deleted, d.Reasons = table.Level, accessBusiness.LevelNone, accessBusiness.LevelNone, true, nil
	return d
}

// RowTitle is a row as a notice names it.
type RowTitle struct {
	Table     uuid.UUID
	TableName string
	// Title is the row's name, as its links show it ("Untitled" when it has
	// none); a row in the trash is "A deleted row".
	Title   string
	Deleted bool
}

// deletedRow is how a row in the trash is named: its cells are its table's
// trash's to show.
const deletedRow = "A deleted row"

// RowTitles names the rows among ids by their tables' naming field (the
// primary field, or the first that names rows), with each table's name. It
// decides nothing about who may see them: the caller does that first. A row
// that isn't there is left out.
func RowTitles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]RowTitle, error) {
	homes, err := rowHomes(ctx, ids)
	if err != nil {
		return nil, err
	}
	byTable := map[uuid.UUID][]uuid.UUID{}
	out := make(map[uuid.UUID]RowTitle, len(homes))
	for id, h := range homes {
		out[id] = RowTitle{Table: h.Table, Title: deletedRow, Deleted: h.Deleted}
		if !h.Deleted {
			byTable[h.Table] = append(byTable[h.Table], id)
		}
	}
	tableIDs := make([]uuid.UUID, 0, len(homes))
	for _, h := range homes {
		tableIDs = append(tableIDs, h.Table)
	}
	tables, err := tablesByIDs(ctx, tableIDs)
	if err != nil {
		return nil, err
	}
	names := make(map[uuid.UUID]string, len(tables))
	for _, t := range tables {
		names[t.Id] = strings.TrimSpace(t.Name)
	}
	for table, rows := range byTable {
		titles, err := rowNames(ctx, table, rows)
		if err != nil {
			return nil, err
		}
		for _, id := range rows {
			if title, ok := titles[id]; ok {
				rt := out[id]
				rt.Title = title
				out[id] = rt
			}
		}
	}
	for id, rt := range out {
		rt.TableName = names[rt.Table]
		if rt.TableName == "" {
			rt.TableName = "Untitled table"
		}
		out[id] = rt
	}
	return out, nil
}

// rowNames is the names of a table's live rows among ids, as links name them
// (labelField, clipLabel): a formula's worked out on a fresh budget.
func rowNames(ctx context.Context, table uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	fields, err := fieldsOf(ctx, table)
	if err != nil {
		return nil, err
	}
	label := labelField(fields)
	out := make(map[uuid.UUID]string, len(ids))
	if label == nil {
		for _, id := range ids {
			out[id] = clipLabel("")
		}
		return out, nil
	}
	keys := []string{label.Id.String()}
	var program *formula.Program
	types := make(map[string]string, len(fields))
	for _, f := range fields {
		types[f.Id.String()] = f.Type
	}
	if label.Type == model.FieldFormula {
		// A formula reads other cells: all of them, for a few rows.
		keys = keys[:0]
		for _, f := range fields {
			keys = append(keys, f.Id.String())
		}
		program = formula.Compile(onlyFormulas(fields, map[string]bool{label.Id.String(): true})).Within(formula.NewBudget())
	}
	rows, err := cellsOf(ctx, table, ids, keys)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, r := range rows {
		pr := newPageRow(r)
		var v formula.Value
		if program != nil {
			v = program.RunRow(formulaRow(r), func(id string) (formula.Value, int) { return pr.cell(id, types[id]) }, now, time.UTC)[label.Id.String()]
		} else {
			v, _ = pr.cell(label.Id.String(), label.Type)
		}
		out[r.Id] = clipLabel(v.Text())
	}
	return out, nil
}
