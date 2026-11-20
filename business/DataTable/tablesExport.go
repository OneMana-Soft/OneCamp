package business

// Every table a reader can open as one Excel workbook (gsheets-import-02:
// POST /tables/export): a sheet for each table, in the tables list's order,
// named as the table is (WriteXLSX makes a name Excel can't hold one it can,
// and its Notes sheet says so), each sheet every live field of its table in
// the table's order and every row its reader reads, in the table's order,
// each cell as a view's export writes it (xlsxExport.go, newTableExport).
//
// Who may: any member, and only for the tables they can open. The list is
// read for them (ListTables), and every table on it is asked again, by
// loadViewable's own rule, in one read for them all (openedAmong), before
// any sheet is made, so a table they can't open has no sheet, not even its
// name, whatever the list says. Its rows are read as theirs (a link into a table they can't open is
// a private row), and an admin, who opens every table, has every sheet, as
// each table's own export lets them.
//
// Bounded: at most maxTablesExported tables and maxTablesExportRows rows in
// all, counted in one read before any of the file is written, so past either
// the export is refused in words, never cut short; a reader who can open no
// table is refused in words too. The rows are read a window at a time as
// each sheet is written, so one window is held at a time.

import (
	"context"
	"fmt"
	"io"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// TablesExport is every table a reader can open, made ready to write as one
// workbook: each checked, its fields read, and the first sheet's first rows
// read, so a refusal comes before any of the file does.
type TablesExport struct {
	tables []*TableExport
	// ids are the tables' ids, a sheet's each, in the workbook's order.
	ids []uuid.UUID
}

var (
	// tablesListed is where an export of every table reads the list of them:
	// a seam, so a test can have the list name a table its reader can't open.
	tablesListed = ListTables
	// maxTablesExported is the most tables one export holds: each is read
	// and counted before the file begins.
	maxTablesExported = 100
	// maxTablesExportRows is the most rows one export of every table holds
	// in all, under the rows one sheet holds in Excel: the whole export is
	// never more work than one big table's.
	maxTablesExportRows = 1_000_000
)

// tablesExportRefused is an export of every table refused in the words its
// reader is shown.
type tablesExportRefused string

func (r tablesExportRefused) Error() string { return string(r) }

// IsTablesExportRefused is an export of every table refused in its own
// words: none to export, or more than one workbook holds (a 422).
func IsTablesExportRefused(err error) bool {
	_, ok := err.(tablesExportRefused)
	return ok
}

// PrepareTablesExport readies actor's export of every table they can open
// (see the file's comment for who may and what each sheet holds). It reads
// the tables, their rows' count and their fields once for them all; each
// sheet's formulas and rows are read as it is written (tableSheet).
func PrepareTablesExport(ctx context.Context, actor Actor) (*TablesExport, error) {
	listed, err := tablesListed(ctx, actor)
	if err != nil {
		return nil, errRowsUnavailable
	}
	ids := make([]uuid.UUID, len(listed))
	for i, l := range listed {
		ids[i] = l.Id
	}
	// Asked again, as one table's export asks: a table the list named by
	// mistake, or one closed to them or gone since, has no sheet at all.
	opened, err := openedAmong(ctx, ids, actor)
	if err != nil {
		return nil, err
	}
	switch {
	case len(opened) == 0:
		return nil, tablesExportRefused("There's no table you can open to export.")
	case len(opened) > maxTablesExported:
		return nil, tablesExportRefused(fmt.Sprintf("You can open more than %s tables, more than one workbook holds. Export the tables one at a time.", helpers.Thousands(int64(maxTablesExported))))
	}
	ids = ids[:0]
	for _, t := range opened {
		ids = append(ids, t.Id)
	}
	n, err := model.CountRowsOfTables(ctx, ids, maxTablesExportRows)
	if err != nil {
		return nil, errRowsUnavailable
	}
	if n > maxTablesExportRows {
		return nil, tablesExportRefused(fmt.Sprintf("Your tables hold more than %s rows together, more than one workbook holds. Export the biggest tables one at a time.", helpers.Thousands(int64(maxTablesExportRows))))
	}
	fields, err := model.FieldsOfTables(ctx, ids)
	if err != nil {
		return nil, errRowsUnavailable
	}
	x := &TablesExport{}
	for _, t := range opened {
		s := &tableSheet{table: t.Id, actor: actor, all: fields[t.Id]}
		s.export = newTableExport(ctx, t, s.all, s.window, nil)
		x.tables = append(x.tables, s.export)
		x.ids = append(x.ids, t.Id)
	}
	first := x.tables[0]
	if first.rows, first.more, err = first.next(ctx); err != nil {
		return nil, unseen(err)
	}
	return x, nil
}

// tableSheet is a table's sheet in an export of every table: every live
// field, and every row in the table's order as its reader reads them. Its
// formulas are planned and its rows read as the sheet is written, a window
// at a time, so one sheet's are held at a time.
type tableSheet struct {
	table  uuid.UUID
	actor  Actor
	all    []*model.Field
	export *TableExport
	rows   rowWindows
}

// window is the sheet's next window of rows, its formulas planned first:
// its rowWindows, as a view's export has viewRows.
func (s *tableSheet) window(ctx context.Context) ([]*model.Row, bool, error) {
	if s.rows == nil {
		s.export.plan(ctx, s.actor, s.all)
		s.rows = rowsFound(s.table, s.actor, query.Spec{})
	}
	return s.rows(ctx)
}

// Tables are the ids of the tables the workbook holds, a sheet each, in its
// order: what the export's row in the admin audit log names.
func (x *TablesExport) Tables() []string {
	out := make([]string, len(x.ids))
	for i, id := range x.ids {
		out[i] = id.String()
	}
	return out
}

// WriteXLSX writes the export to w as one Excel workbook, a sheet a table,
// and answers what was cut to Excel's limits, which its Notes sheet says
// too.
func (x *TablesExport) WriteXLSX(ctx context.Context, w io.Writer) ([]string, error) {
	return writeWorkbook(ctx, w, x.tables)
}
