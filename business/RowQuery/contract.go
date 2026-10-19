// Package business (RowQuery) answers questions about a table's rows on the
// server (design/tables-vs-airtable/PROGRAM.md §4.2 #63; A3 builds it): windows
// of rows in a view's order, counts, groups, aggregates, search and exports,
// over every row the reader may see, compiled from the shared query spec (#80,
// business/QuerySpec, which projects owns).
//
// DAY 0. A window is today's page (business/DataTable ListRows: offset pages in
// the table's order, position then age then id), its rows worked out for the
// reader; a count is today's (CountRows). The cursor is opaque, so A3's keyset
// cursors replace the offsets without a caller changing. No spec is taken: A3
// adds the compiler, registered with business/QuerySpec for the scope "table".
// business/DataTable registers today's engine at init. Nothing calls it yet, so
// nothing changes by its existing.
package business

import (
	"context"
	"errors"

	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// Cursor is where a window starts; "" is the first row. Opaque to callers.
type Cursor string

// Window is some of a table's rows, in order.
type Window struct {
	Rows []*model.Row `json:"rows"`
	// Next is where the window after this one starts; "" past the last row.
	Next Cursor `json:"next"`
}

// Engine reads a table's rows for someone.
type Engine interface {
	// Window is up to n rows from a cursor, worked out for who.
	Window(ctx context.Context, who tableAccess.Who, table uuid.UUID, from Cursor, n int) (*Window, error)
	// Count is how many rows the table has that who may see.
	Count(ctx context.Context, who tableAccess.Who, table uuid.UUID) (int, error)
}

// ErrNotRegistered is a read asked for before business/DataTable registered
// the engine.
var ErrNotRegistered = errors.New("row query: no engine is registered")

// Rows is the engine everything reads rows through. business/DataTable
// registers today's at init.
var Rows Engine = noEngine{}

type noEngine struct{}

func (noEngine) Window(context.Context, tableAccess.Who, uuid.UUID, Cursor, int) (*Window, error) {
	return nil, ErrNotRegistered
}

func (noEngine) Count(context.Context, tableAccess.Who, uuid.UUID) (int, error) {
	return 0, ErrNotRegistered
}

// SweepCellIndexRepair is the sweep A3 registers to rebuild what its cell
// index is missing.
const SweepCellIndexRepair = "tables.cellindex.repair"
