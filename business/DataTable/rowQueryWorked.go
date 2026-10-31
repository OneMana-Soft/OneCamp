package business

// Row Query's view of formulas and rollups (tables A3): a spec that filters
// or sorts by one is answered over rows worked out as the reader reads them,
// until A4 stores their values. Row Query can't import this package, so it
// asks through rowQuery.WorkedOut, set here.

import (
	"context"

	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func init() {
	rowQuery.WorkedOut = workedOut
}

// workedOut is up to max of a table's rows in its order, their formulas and
// rollups worked out for who, more when the table has more, and what each
// formula and rollup gives (number, text, date or checkbox).
func workedOut(ctx context.Context, who tableAccess.Who, table uuid.UUID, fields []*model.Field, max int) ([]*model.Row, bool, map[string]string, error) {
	a := actorOf(who)
	ctx = asViewer(ctx, a)
	var rows []*model.Row
	key, after := "", uuid.Nil
	for len(rows) <= max {
		page, err := model.ListRowsAfter(ctx, table, key, after, maxWindow)
		if err != nil {
			return nil, false, nil, errRowsUnavailable
		}
		rows = append(rows, page...)
		if len(page) < maxWindow {
			break
		}
		last := page[len(page)-1]
		key, after = last.PositionKey, last.Id
	}
	more := len(rows) > max
	if more {
		return nil, true, nil, nil
	}
	rows, err := readWithComputed(ctx, table, rows)
	if err != nil {
		return nil, false, nil, err
	}
	return rows, false, resultKinds(planComputed(ctx, fields).program, fields), nil
}
