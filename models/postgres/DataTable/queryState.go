package models

import (
	"context"
	"errors"

	tableDomain "github.com/akashc777/OneCamp/domain/DataTable"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// QueryState is what a rows query reads of its table before its window
// (business/RowQuery Run): its live fields, in order; whether its cell index
// is whole, and the version that made it so (CellIndexState); and whether a
// live row has no place key yet, which a window's order then can't follow.
type QueryState struct {
	Fields  []*Field
	Whole   bool
	Version int
	Unkeyed bool
}

// ReadQueryState reads a table's QueryState in one round trip.
func ReadQueryState(ctx context.Context, table uuid.UUID) (QueryState, error) {
	var st QueryState
	var index []indexState
	var unkeyed []bool
	fields := pgexec.Rows(&st.Fields, func(r pgexec.Row) (*Field, error) { return scanField(r) }, tableDomain.LiveFieldsSQL, table)
	state := pgexec.Rows(&index, scanIndexState, tableDomain.CellIndexStateSQL, table)
	keys := pgexec.Rows(&unkeyed, func(r pgexec.Row) (has bool, err error) {
		err = r.Scan(&has)
		return has, err
	}, tableDomain.HasUnkeyedRowsSQL, table)
	pgexec.Batch(ctx, fields, state, keys)
	if err := errors.Join(fields.Err, state.Err, keys.Err); err != nil {
		helpers.LogErrorWithContext(ctx, "models/ReadQueryState err: %+v", err)
		return QueryState{}, err
	}
	if len(index) > 0 {
		st.Whole, st.Version = index[0].at.Valid, index[0].version
	}
	st.Unkeyed = len(unkeyed) > 0 && unkeyed[0]
	return st, nil
}
