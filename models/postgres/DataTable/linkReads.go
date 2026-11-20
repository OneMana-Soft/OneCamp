package models

import (
	"bytes"
	"context"
	"database/sql"
	"sort"
	"time"

	tableDomain "github.com/akashc777/OneCamp/domain/DataTable"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// LinkRead is one relation's side of a page: the field that keeps the
// links, whether the rows are the links' to end, the rows, and (for
// LinksOfMany) how many links of each to read.
type LinkRead struct {
	Field  uuid.UUID
	To     bool
	Rows   []uuid.UUID
	Limits []int
}

// linkArgs is the reads as CountLinksSQL and LinksOfSQL take them: each
// side's read index, field, row and limit.
func linkArgs(reads []LinkRead, limits bool) []any {
	var side [2]struct {
		i, k    []int64
		f, rows []uuid.UUID
	}
	for i, r := range reads {
		s := &side[0]
		if r.To {
			s = &side[1]
		}
		for j, row := range r.Rows {
			s.i, s.f, s.rows = append(s.i, int64(i)), append(s.f, r.Field), append(s.rows, row)
			if limits {
				s.k = append(s.k, int64(r.Limits[j]))
			}
		}
	}
	var out []any
	for _, s := range side {
		out = append(out, pq.Array(s.i), pq.Array(s.f), pq.Array(s.rows))
		if limits {
			out = append(out, pq.Array(s.k))
		}
	}
	return out
}

// anyRows says whether any read asks for a row.
func anyRows(reads []LinkRead) bool {
	for _, r := range reads {
		if len(r.Rows) > 0 {
			return true
		}
	}
	return false
}

// CountLinksMany counts, up to upTo, the links each read's rows hold, in one
// statement: out[i] is reads[i]'s.
func CountLinksMany(ctx context.Context, reads []LinkRead, upTo int) ([]map[uuid.UUID]int, error) {
	out := make([]map[uuid.UUID]int, len(reads))
	for i, r := range reads {
		out[i] = make(map[uuid.UUID]int, len(r.Rows))
	}
	if !anyRows(reads) {
		return out, nil
	}
	type count struct {
		i   int
		row uuid.UUID
		n   int
	}
	got, err := pgexec.Query(ctx, tableDomain.CountLinksSQL, func(r pgexec.Row) (c count, err error) {
		err = r.Scan(&c.i, &c.row, &c.n)
		return c, err
	}, append(linkArgs(reads, false), upTo)...)
	if err != nil {
		return nil, err
	}
	for _, c := range got {
		out[c.i][c.row] = c.n
	}
	return out, nil
}

// LinksOfMany reads the first Limits[j] rows each read's row j links to, in
// one statement, each row's in the order its links were made: out[i] is
// reads[i]'s. A row with no links has no entry.
func LinksOfMany(ctx context.Context, reads []LinkRead) ([]map[uuid.UUID][]uuid.UUID, error) {
	out := make([]map[uuid.UUID][]uuid.UUID, len(reads))
	for i, r := range reads {
		out[i] = make(map[uuid.UUID][]uuid.UUID, len(r.Rows))
	}
	if !anyRows(reads) {
		return out, nil
	}
	type link struct {
		i   int
		row uuid.UUID
		m   madeLink
	}
	got, err := pgexec.Query(ctx, tableDomain.LinksOfSQL, func(r pgexec.Row) (l link, err error) {
		err = r.Scan(&l.i, &l.row, &l.m.other, &l.m.at)
		return l, err
	}, linkArgs(reads, true)...)
	if err != nil {
		return nil, err
	}
	found := make([]map[uuid.UUID][]madeLink, len(reads))
	for _, l := range got {
		if found[l.i] == nil {
			found[l.i] = map[uuid.UUID][]madeLink{}
		}
		found[l.i][l.row] = append(found[l.i][l.row], l.m)
	}
	for i := range found {
		orderLinks(found[i], out[i])
	}
	return out, nil
}

// LinksOf is LinksOfMany for one field's rows (to them, when to is set): the
// first limits[i] rows row i links to, by row.
func LinksOf(ctx context.Context, fieldId uuid.UUID, rows []uuid.UUID, limits []int, to bool) (map[uuid.UUID][]uuid.UUID, error) {
	got, err := LinksOfMany(ctx, []LinkRead{{Field: fieldId, To: to, Rows: rows, Limits: limits}})
	if err != nil {
		return nil, err
	}
	return got[0], nil
}

// CellsRead is the rows of one table a page links to, and the cells of them
// it reads.
type CellsRead struct {
	Table uuid.UUID
	IDs   []uuid.UUID
	Keys  []string
}

// CellsOfMany reads the live rows among each read's IDs, in no particular
// order, each with only its id, its cells of Keys, and what a field worked
// out from the row reads (its number, when it was made and last changed, and
// by whom), for every table's rows in one statement: out[i] is reads[i]'s.
func CellsOfMany(ctx context.Context, reads []CellsRead) ([][]*Row, error) {
	out := make([][]*Row, len(reads))
	at := make(map[uuid.UUID]int, len(reads))
	var ids, tables, keyTables []uuid.UUID
	var keys []string
	for i, r := range reads {
		at[r.Table] = i
		for _, id := range r.IDs {
			ids, tables = append(ids, id), append(tables, r.Table)
		}
		for _, k := range r.Keys {
			keys, keyTables = append(keys, k), append(keyTables, r.Table)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	// The lists go to pgx as they are: through pq.Array, pgx writes out the
	// whole list in an error before taking it, megabytes for a first batch of
	// up to 2,000 rows a table.
	rows, err := pgexec.Query(ctx, tableDomain.CellsOfSQL, func(s pgexec.Row) (*Row, error) {
		r := &Row{}
		var rowNo sql.NullInt64
		var createdBy, updatedBy uuid.NullUUID
		if err := s.Scan(&r.TableId, &r.Id, &rowNo, &createdBy, &r.CreatedAt, &r.UpdatedAt, &updatedBy, &r.Values); err != nil {
			return nil, err
		}
		r.RowNo = rowNo.Int64
		if createdBy.Valid {
			r.CreatedBy = &createdBy.UUID
		}
		if updatedBy.Valid {
			r.UpdatedBy = &updatedBy.UUID
		}
		return r, nil
	}, ids, tables, keys, keyTables)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		i := at[r.TableId]
		out[i] = append(out[i], r)
	}
	return out, nil
}

// CellsOf is CellsOfMany for one table's rows among ids, with its cells of
// keys.
func CellsOf(ctx context.Context, tableId uuid.UUID, ids []uuid.UUID, keys []string) ([]*Row, error) {
	got, err := CellsOfMany(ctx, []CellsRead{{Table: tableId, IDs: ids, Keys: keys}})
	if err != nil {
		return nil, err
	}
	return got[0], nil
}

// PlanOpenTables reads the live tables among ids and the live fields of
// fieldsOf, as reads a caller runs beside its own in one round trip
// (pgexec.Together): once they have run, done gets them (a table that's gone
// has no entry, and fields[t] is in the order ListFields gives).
func PlanOpenTables(ids, fieldsOf []uuid.UUID, done func(map[uuid.UUID]*DataTable, map[uuid.UUID][]*Field, error)) pgexec.Planned {
	if len(ids) == 0 {
		return pgexec.Planned{Then: func() { done(map[uuid.UUID]*DataTable{}, map[uuid.UUID][]*Field{}, nil) }}
	}
	var ts []*DataTable
	var fs []*Field
	readTables := pgexec.Rows(&ts, func(r pgexec.Row) (*DataTable, error) { return scanTable(r) }, tableDomain.OpenTablesSQL, pq.Array(ids))
	var readFields *pgexec.Read
	if len(fieldsOf) > 0 {
		readFields = pgexec.Rows(&fs, func(r pgexec.Row) (*Field, error) { return scanField(r) }, tableDomain.FieldsOfTablesSQL, pq.Array(fieldsOf))
	}
	return pgexec.Planned{Reads: []*pgexec.Read{readTables, readFields}, Then: func() {
		if readTables.Err != nil {
			done(nil, nil, readTables.Err)
			return
		}
		if readFields != nil && readFields.Err != nil {
			done(nil, nil, readFields.Err)
			return
		}
		tables, fields := map[uuid.UUID]*DataTable{}, map[uuid.UUID][]*Field{}
		for _, t := range ts {
			tables[t.Id] = t
		}
		for _, f := range fs {
			fields[f.TableId] = append(fields[f.TableId], f)
		}
		done(tables, fields, nil)
	}}
}

// madeLink is a link read from one row: the row at its other end, and when
// it was made.
type madeLink struct {
	other uuid.UUID
	at    time.Time
}

// orderLinks puts each row's links in the order they were made (then by the
// other row's id) into out: sorting them all in the query takes longer than
// reading them.
func orderLinks(found map[uuid.UUID][]madeLink, out map[uuid.UUID][]uuid.UUID) {
	for row, ms := range found {
		sort.Slice(ms, func(i, j int) bool {
			if !ms[i].at.Equal(ms[j].at) {
				return ms[i].at.Before(ms[j].at)
			}
			return bytes.Compare(ms[i].other[:], ms[j].other[:]) < 0
		})
		ids := make([]uuid.UUID, len(ms))
		for i, m := range ms {
			ids[i] = m.other
		}
		out[row] = ids
	}
}
