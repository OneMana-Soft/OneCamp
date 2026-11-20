package business

// A window of a table's rows for a spec (tables A3, POST /tables/{id}/query):
// Row Query finds which rows and in what order, on the server, over every row
// the reader may see; here they are read whole and worked out for the reader,
// in that order.

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// RowsWindow is one window of a spec's rows.
type RowsWindow struct {
	Rows []*model.Row `json:"rows"`
	// Next is the next window's cursor; none past the last.
	Next query.Cursor `json:"next,omitempty"`
	// At is the place its first row holds in the spec's order, for a window
	// read at a place (query.Page At): a reader puts its rows there only when
	// the answer says so, never a first window a server without jumps sent.
	At int `json:"at,omitempty"`
	// Total is how many rows meet the spec, on the first window alone (the
	// reader keeps it); TotalCapped says it counted only so far
	// (rowQuery.CountCap) and there are more.
	Total       *int `json:"total,omitempty"`
	TotalCapped bool `json:"total_capped,omitempty"`
	// Summaries are the spec's summaries over every row it keeps, on the
	// first window alone, as Total is.
	Summaries query.Summaries `json:"summaries,omitempty"`
	// Groups are the spec's groups, every level's, depth first in their
	// order, each with its count and summaries over every row it keeps (a
	// collapsed group's too), on the first window alone, as Total is.
	Groups []query.GroupCount `json:"groups,omitempty"`
}

// QueryRows answers a window of spec over table, for actor, in the reader's
// zone (WithZone's, as ?tz= gives it); a first window counts its set too when
// count is set (rowQuery.WithCount: a reader asks for it in a request of its
// own, beside its first window).
func QueryRows(ctx context.Context, table uuid.UUID, actor Actor, spec query.Spec, page query.Page, count bool) (*RowsWindow, error) {
	spec.Scope = query.Scope{Kind: query.ScopeTable, ID: table.String()}
	who := accessBusiness.Subject{UserUUID: actor.UserID.String(), Admin: actor.IsAdmin}
	qctx := rowQuery.WithZone(ctx, zoneFrom(ctx))
	if count {
		qctx = rowQuery.WithCount(qctx)
	}
	qctx, held := rowQuery.WithHeld(qctx)
	res, err := query.Queries.Run(qctx, who, spec, page)
	switch {
	case errors.Is(err, rowQuery.ErrNotFound):
		return nil, errNotFound
	case err != nil:
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(res.IDs))
	for _, s := range res.IDs {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	rows, err := windowRows(asViewer(ctx, actor), table, ids, held)
	if err != nil {
		return nil, err
	}
	w := &RowsWindow{Rows: rows, Next: res.Next, At: page.At, Summaries: res.Summaries, Groups: res.Groups}
	if res.Total != rowQuery.NotCounted {
		total := res.Total
		if total > rowQuery.CountCap {
			total, w.TotalCapped = rowQuery.CountCap, true
		}
		w.Total = &total
	}
	return w, nil
}

// windowRows is a window's rows, ids in order, without those deleted since
// they were found, with their computed cells worked out for the reader ctx
// carries. With the fields Run held, the rows are read in the round trip that
// opens the tables those fields link to; a Runner that held none has the
// rows read, and the fields, on their own.
func windowRows(ctx context.Context, table uuid.UUID, ids []uuid.UUID, held *rowQuery.Held) ([]*model.Row, error) {
	if len(ids) == 0 {
		return []*model.Row{}, nil
	}
	if !held.Read {
		found, err := model.RowsByIDs(ctx, table, ids)
		if err != nil {
			return nil, errRowsUnavailable
		}
		return readWithComputed(ctx, table, inOrder(found, ids))
	}
	var found []*model.Row
	var err error
	c := planAlong(ctx, held.Fields, model.PlanRowsByIDs(ctx, table, ids, func(rows []*model.Row, e error) { found, err = rows, e }))
	if err != nil {
		return nil, errRowsUnavailable
	}
	rows := inOrder(found, ids)
	if len(rows) > 0 {
		c.apply(ctx, rows)
	}
	return rows, nil
}
