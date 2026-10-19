// Package business (ComputedStore) keeps formula, rollup, lookup and count
// values stored and current (design/tables-vs-airtable/PROGRAM.md §4.2 #64; A4
// builds it), so the server can sort, filter and group by them at 100,000 rows,
// read with an "as of" stamp.
//
// DAY 0. Every computed value is worked out on each read, as today
// (business/DataTable withComputed: links within their per-read limits,
// rollups, formulas within their step budget), for the person reading.
// business/DataTable registers that at init. Nothing calls Values yet, so
// nothing changes by its existing.
package business

import (
	"context"
	"errors"

	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// Store fills rows' computed cells for a reader.
type Store interface {
	// Read puts each computed field's value into rows (of one table, whose
	// fields are fields), as who may see it.
	Read(ctx context.Context, who tableAccess.Who, fields []*model.Field, rows []*model.Row) error
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

// SweepRecompute is the sweep A4 registers to work out what its dirty marks
// name.
const SweepRecompute = "tables.computed.recompute"
