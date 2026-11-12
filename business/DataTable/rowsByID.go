package business

// Rows by id (tables A7, Live Rows #65): a live message names the rows a
// change set touched, by id, and an open view fetches those rows alone
// (GET /tables/{id}/rows?ids=) instead of its whole window or the bundle. The
// same read as a page of rows (ListRows): the table opened for the reader,
// each row worked out for them (formulas, rollups, links).

import (
	"context"
	"encoding/json"
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

// ErrBadFieldIDs is a ?fields= that isn't a list of field ids: empty, too
// long, or with something that isn't an id in it.
var ErrBadFieldIDs = errors.New("rows: name 1 to 500 field ids, separated by commas")

// ParseRowIDs reads ?ids= (comma-separated row ids), each once, in order.
func ParseRowIDs(s string) ([]uuid.UUID, error) { return parseIDList(s, ErrBadRowIDs) }

// ParseFieldIDs reads ?fields= (comma-separated field ids), each once, in
// order: the cells a live message named (tables L1).
func ParseFieldIDs(s string) ([]uuid.UUID, error) { return parseIDList(s, ErrBadFieldIDs) }

// parseIDList reads 1 to MaxRowsByID comma-separated ids, each once, in
// order, or answers bad.
func parseIDList(s string, bad error) ([]uuid.UUID, error) {
	var out []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		id, err := uuid.Parse(p)
		if err != nil {
			return nil, bad
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 || len(out) > MaxRowsByID {
		return nil, bad
	}
	return out, nil
}

// RowsByID is the rows of table named by ids that are there (not deleted),
// in the order named, each as actor may see it. A row deleted since, or never
// the table's, is left out: the reader drops it from what it holds.
func RowsByID(ctx context.Context, table uuid.UUID, actor Actor, ids []uuid.UUID) ([]*model.Row, error) {
	rows, _, err := rowsByID(ctx, table, actor, ids)
	return rows, err
}

// rowsByID is RowsByID with the table's fields it read to work the rows out.
func rowsByID(ctx context.Context, table uuid.UUID, actor Actor, ids []uuid.UUID) ([]*model.Row, []*model.Field, error) {
	if len(ids) == 0 || len(ids) > MaxRowsByID {
		return nil, nil, ErrBadRowIDs
	}
	ctx = asViewer(ctx, actor)
	if _, err := loadViewable(ctx, table, actor); err != nil {
		return nil, nil, err
	}
	found, err := model.RowsByIDs(ctx, table, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DataTable RowsByID: %v", err)
		return nil, nil, errRowsUnavailable
	}
	return readWorkedOut(ctx, table, inOrder(found, ids))
}

// CellsByID is RowsByID narrowed to the cells a live message named (tables
// L1, Live Rows #65): an open view reads the cells that changed, under its
// reader's own access, and puts them in the rows it holds, never reading its
// windows again. The message carries ids alone, as every reader of a table's
// topic gets the same message; the values come here, read as actor.
func CellsByID(ctx context.Context, table uuid.UUID, actor Actor, ids, fields []uuid.UUID) ([]*model.Row, error) {
	rows, all, err := rowsByID(ctx, table, actor, ids)
	if err != nil || len(rows) == 0 {
		return rows, err
	}
	return narrowCells(all, rows, fields), nil
}

// narrowCells keeps in each row's values the cells of the fields named that
// the table has, and of every field worked out on each read (readsWorkOut),
// which may follow from them; a kept cell that is empty is null, so the
// reader clears it where it holds the row. Pure.
func narrowCells(fields []*model.Field, rows []*model.Row, named []uuid.UUID) []*model.Row {
	asked := make(map[uuid.UUID]bool, len(named))
	for _, id := range named {
		asked[id] = true
	}
	var keep []string
	for _, f := range fields {
		if asked[f.Id] || readsWorkOut(f) {
			keep = append(keep, f.Id.String())
		}
	}
	for _, r := range rows {
		var cells map[string]json.RawMessage
		if json.Unmarshal([]byte(r.Values), &cells) != nil {
			cells = nil
		}
		out := make(map[string]json.RawMessage, len(keep))
		for _, k := range keep {
			if v, ok := cells[k]; ok {
				out[k] = v
			} else {
				out[k] = json.RawMessage("null")
			}
		}
		b, _ := json.Marshal(out)
		r.Values = string(b)
	}
	return rows
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
