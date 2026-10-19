// Package business (Base) is where tables live (design/tables-vs-airtable/
// PROGRAM.md §4.2 #67 and D-TB-2; A5 builds it): a base holds related tables,
// their members, interfaces, automations and forms, and is what imports,
// snapshots and duplicates work on.
//
// DAY 0. There are no bases: every table is in the workspace's one, and BaseOf
// answers DefaultBase for any table. A5's migration makes that base, "Tables",
// with DefaultBase as its id, so the answer doesn't change when bases exist.
// Nothing calls Bases yet, so nothing changes by its existing.
package business

import (
	"context"

	"github.com/google/uuid"
)

// DefaultBase is the workspace's one base until A5 makes bases, and the id
// A5's migration gives the "Tables" base every existing table moves into.
var DefaultBase = uuid.MustParse("7ab1e5ba-5e00-4000-8000-000000000001")

// Finder says which base a table is in.
type Finder interface {
	BaseOf(ctx context.Context, table uuid.UUID) (uuid.UUID, error)
}

// Bases is the finder everything asks.
var Bases Finder = oneBase{}

// oneBase is today: one base for every table.
type oneBase struct{}

func (oneBase) BaseOf(context.Context, uuid.UUID) (uuid.UUID, error) { return DefaultBase, nil }

// SweepSyncRun is the sweep B9 registers to run tables' one-way syncs.
const SweepSyncRun = "tables.sync.run"
