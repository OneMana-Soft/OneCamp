// Package business (TableAccess) decides what someone may do with a table
// (design/tables-vs-airtable/PROGRAM.md §4.2 #66; A5 builds it): read its rows,
// write them, and change the table itself, on the Access Resolver with
// Airtable's roles, field rules and row rules.
//
// DAY 0. The rule is today's (business/DataTable canView, canManage and the
// agent asker rule, opens): an admin, the table's creator, or anyone in the
// workspace for a table shown to the workspace may read it and write its rows;
// an admin or the creator may change it. business/DataTable registers that rule
// here at init, and the Access Resolver's "table" kind decides by it, so A5 can
// move the rule behind the same names without a caller changing. Nothing asks
// Tables yet, so nothing changes by its existing.
package business

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// The Access Resolver's kinds for tables. KindTable is registered at day 0;
// A5 registers the others (a base, a table's field, a table's row).
const (
	KindTable      = "table"
	KindTableField = "table_field"
	KindTableRow   = "table_row"
	KindBase       = "base"
)

// Who is the person a decision is for, never taken from what a client sends:
// the signed-in member or the token's owner, and the agent acting for them.
type Who struct {
	User  uuid.UUID
	Admin bool
	// Agent is the agent acting for User; uuid.Nil when they act themselves.
	Agent uuid.UUID
}

// Decision is what someone may do with one table.
type Decision struct {
	// View is reading the table, its fields and its rows.
	View bool
	// Edit is writing its rows: cells, new rows, deleting rows.
	Edit bool
	// Manage is changing the table itself: fields, views, its settings,
	// deleting it.
	Manage bool
}

// Rules decides tables.
type Rules interface {
	// Resolve is what who may do with table. A table that isn't there, or is
	// deleted, is ErrNotFound.
	Resolve(ctx context.Context, who Who, table uuid.UUID) (Decision, error)
}

var (
	// ErrNotFound is a table that isn't there, or is in the trash.
	ErrNotFound = errors.New("table access: that table isn't there")
	// ErrNotRegistered is a decision asked for before any rule was registered:
	// nothing is allowed by it.
	ErrNotRegistered = errors.New("table access: no rule is registered")
)

// Tables is the rule everything asks. business/DataTable registers today's
// at init.
var Tables Rules = noRule{}

// noRule allows nothing: a binary without business/DataTable has no tables.
type noRule struct{}

func (noRule) Resolve(context.Context, Who, uuid.UUID) (Decision, error) {
	return Decision{}, ErrNotRegistered
}
