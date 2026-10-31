// Package business (ComputedStore) keeps formula, rollup, lookup and count
// values stored and current (design/tables-vs-airtable/PROGRAM.md §4.2 #64; A4
// builds it), so the server can sort, filter and group by them at 100,000 rows,
// read with an "as of" stamp.
//
// Every computed value is worked out on each read (business/DataTable
// withComputed: links within their per-read limits, rollups, formulas within
// their step budget), for the person reading. The store (airtable-03) also
// keeps a formula's or a rollup's values in the cell index, so business/RowQuery
// sorts, filters, groups and adds up by them along it; a kept value answers a
// reader only when Serves says it is what their own read works out.
// business/DataTable registers both at init.
package business

import (
	"context"
	"errors"

	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// Store fills rows' computed cells for a reader.
type Store interface {
	// Read puts each computed field's value into rows (of one table, whose
	// fields are fields), as who may see it.
	Read(ctx context.Context, who tableAccess.Who, fields []*model.Field, rows []*model.Row) error
	// Serves is what each of a table's formula and rollup fields named by
	// id gives ("number", "text" or "checkbox"), when the store may answer
	// who by every one of them: each never reads the clock or the reader's
	// zone, can be worked out, and every table its values reach, each level
	// of its rollups and linked reads, is one who opens, so its kept value is
	// what who's own read works out. Otherwise nil: who is answered by their
	// own read. An error is a no, never a kept value.
	Serves(ctx context.Context, who tableAccess.Who, table uuid.UUID, ids []string) (map[string]string, error)
	// Work is the store's fields of a table worked out for some of its rows
	// with every table open, for the store to keep; with no rows, what each
	// formula and rollup reaches, for a fill to start from.
	Work(ctx context.Context, table uuid.UUID, rows []uuid.UUID) (*Worked, error)
}

// Worked is rows with their formulas and rollups worked out (in each row's
// values) and what each field the store keeps gives; a field the store
// doesn't keep (one that reads the clock, the reader's zone or a date, or
// can't be worked out) isn't in Kinds. Reaches, given only when no rows are
// asked for, is the tables each formula's and rollup's values reach, kept or
// not.
type Worked struct {
	Rows    []*model.Row
	Kinds   map[string]string
	Reaches map[string][]uuid.UUID
}

// ErrNotRegistered is a read asked for before business/DataTable registered
// the store.
var ErrNotRegistered = errors.New("computed store: no store is registered")

// Values is the store every read asks. business/DataTable registers today's
// (worked out on read) at init.
var Values Store = noStore{}

type noStore struct{}

func (noStore) Read(context.Context, tableAccess.Who, []*model.Field, []*model.Row) error {
	return ErrNotRegistered
}

func (noStore) Serves(context.Context, tableAccess.Who, uuid.UUID, []string) (map[string]string, error) {
	return nil, ErrNotRegistered
}

func (noStore) Work(context.Context, uuid.UUID, []uuid.UUID) (*Worked, error) {
	return nil, ErrNotRegistered
}

// SweepRecompute is business/RowQuery's sweep that fills the store's fields
// whose entries aren't current and works out again the rows its dirty marks
// name.
const SweepRecompute = "tables.computed.recompute"
