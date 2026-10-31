package business

// Rows by id (tables A7, Live Rows #65): a live message names the rows a
// change set touched, by id, and an open view fetches those rows alone
// (GET /tables/{id}/rows?ids=) instead of its whole window or the bundle. The
// same read as a page of rows (ListRows): the table opened for the reader,
// each row worked out for them (formulas, rollups, links).

import (
	"context"
	"errors"
	"strings"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// MaxRowsByID is the most rows one read by id names: a window's worth, as a
// live message names at most rowsTold.
const MaxRowsByID = query.MaxPage

// ErrBadRowIDs is a list of ids that isn't one: empty, too long, or with
// something that isn't a row id in it.
var ErrBadRowIDs = errors.New("rows: name 1 to 500 row ids, separated by commas")

// ParseRowIDs reads ?ids= (comma-separated row ids), each once, in order.
func ParseRowIDs(s string) ([]uuid.UUID, error) {
	var out []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		id, err := uuid.Parse(p)
		if err != nil {
			return nil, ErrBadRowIDs
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 || len(out) > MaxRowsByID {
		return nil, ErrBadRowIDs
	}
	return out, nil
}

// RowsByID is the rows of table named by ids that are there (not deleted),
// in the order named, each as actor may see it. A row deleted since, or never
// the table's, is left out: the reader drops it from what it holds.
func RowsByID(ctx context.Context, table uuid.UUID, actor Actor, ids []uuid.UUID) ([]*model.Row, error) {
	if len(ids) == 0 || len(ids) > MaxRowsByID {
		return nil, ErrBadRowIDs
	}
	ctx = asViewer(ctx, actor)
	if _, err := loadViewable(ctx, table, actor); err != nil {
		return nil, err
	}
	found, err := model.RowsByIDs(ctx, table, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DataTable RowsByID: %v", err)
		return nil, errRowsUnavailable
	}
	return readWithComputed(ctx, table, inOrder(found, ids))
}

// RowAsRead is a row just written as a read of it answers: worked out for
// the reader, with its comment count and its page, so a view can put it where
// it holds the row without reading the table again (tables A7: a save
// fetched the whole bundle again, 534 ms at 100,000 rows). The row as written
// when the read fails or the reader can no longer see it.
func RowAsRead(ctx context.Context, table uuid.UUID, actor Actor, written *model.Row) *model.Row {
	if written == nil {
		return nil
	}
	rows, err := RowsByID(ctx, table, actor, []uuid.UUID{written.Id})
	if err != nil || len(rows) != 1 {
		return written
	}
	WithCommentCounts(ctx, rows)
	WithPages(ctx, rows)
	return rows[0]
}

// inOrder is rows in the order of ids, without those not among them. Pure.
func inOrder(rows []*model.Row, ids []uuid.UUID) []*model.Row {
	byID := make(map[uuid.UUID]*model.Row, len(rows))
	for _, r := range rows {
		byID[r.Id] = r
	}
	out := make([]*model.Row, 0, len(ids))
	for _, id := range ids {
		if r := byID[id]; r != nil {
			out = append(out, r)
		}
	}
	return out
}
