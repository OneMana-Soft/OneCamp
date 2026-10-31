package business

// A view's order that no index walks, listed once (tables T5d). A spec no
// walk answers (three sorts, a sort that isn't walked, a search, or a walk
// past its budget) was compile.go's one query on every window: Postgres
// sorted every matching row each time (three sorts took 135 ms a window at
// 100,000 rows). Its first window still is, so a reader who opens a view
// and goes no further pays nothing more. The second lists the order from
// there on, once, as row ids, up to listMax (TableRowList, in Redis), and
// the windows after it read slices of the list.
//
// A list is the order as it was listed. Each committed write to the table's
// rows replaces the table's token (TableRowsToken, the row ledger's
// "tables.row_lists" observer), and so does a change to its links made from
// a linked table, whose ledger isn't this table's (ListsStale, when the
// table's readers are told to look again). The fields' stamp changes with a
// field (an option moved reorders a choice with no row written). A window
// whose list is stale, gone or read through is listed again from the one
// query's keys of the last row shown, which its cursor carries: the rows go
// on as the one query would from there, whatever was written since.
//
// Without Redis every window is the one query's, as before.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// listMax is the most ids one list holds (320 KB in Redis); the rows past
// them are listed again from its last, when a window reaches them.
const listMax = 20000

// idSize is the bytes an id takes in a list.
const idSize = 16

func init() {
	rowLedger.Observers.Register(rowLedger.Observer{Name: "tables.row_lists", Committed: listsStale})
}

// listsStale makes a table's lists stale once a write to its rows commits.
func listsStale(ctx context.Context, cs *rowLedger.ChangeSet) {
	if len(cs.Changes) > 0 {
		ListsStale(ctx, cs.Table)
	}
}

// ListsStale replaces each table's token, so every list of its rows made
// under the old one is stale: for a change its own ledger doesn't see, as
// links a linked table changed, which its filters on links read. A failure
// is the store's to log; the lists made before it then live out their TTL.
func ListsStale(ctx context.Context, tables ...uuid.UUID) {
	tokens := make([]store.StringSet, len(tables))
	for i, t := range tables {
		tokens[i] = store.StringSet{Key: store.CounterKey{Spec: registry.TableRowsToken, Args: []string{t.String()}}, Value: uuid.NewString()}
	}
	_ = store.SetStringMany(ctx, tokens)
}

// listing is a spec no walk answers, as one table's reader asks it.
type listing struct {
	table   uuid.UUID
	fields  []*model.Field
	cols    map[string]*column
	spec    query.Spec
	where   string
	a       *sqlArgs
	who     asker
	unkeyed bool
}

// listCursor is a window's place in a list: its key under its view, the
// entry it starts at, whether the list stops short of the view's end, what
// it was listed under (fresh) and for (view), and the one query's cursor
// after the row shown last, to list again from.
type listCursor struct {
	key   string
	at    int
	cut   bool
	fresh string
	view  string
	after query.Cursor
}

func (c listCursor) cursor() query.Cursor {
	return cursorOf("list", []interface{}{c.key, float64(c.at), c.cut, c.fresh, c.view, string(c.after)})
}

func readListCursor(c query.Cursor) (listCursor, error) {
	v, err := readCursor(c, "list", 6)
	if err != nil {
		return listCursor{}, err
	}
	key, okKey := v[0].(string)
	at, okAt := v[1].(float64)
	cut, okCut := v[2].(bool)
	fresh, okFresh := v[3].(string)
	view, okView := v[4].(string)
	after, okAfter := v[5].(string)
	if !okKey || !okAt || !okCut || !okFresh || !okView || !okAfter || at < 0 || cursorKind(query.Cursor(after)) != "sql" {
		return listCursor{}, ErrBadCursor
	}
	return listCursor{key: key, at: int(at), cut: cut, fresh: fresh, view: view, after: query.Cursor(after)}, nil
}

// stamp is a short hash of parts.
func stamp(parts ...interface{}) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// view is what a list is of: the spec's rows and order, for its reader in
// their zone. A cursor for another is refused. Groups (and the groups the
// reader collapsed) are stamped only when the spec has them, so a list made
// before they were stays its spec's.
func (l listing) view() string {
	parts := []interface{}{l.table, l.spec.Filter, l.spec.Sorts, l.spec.Search, l.who.user, l.who.agent, l.who.admin, l.who.zone.String()}
	if len(l.spec.Groups) > 0 {
		parts = append(parts, l.spec.Groups)
	}
	return stamp(parts...)
}

// fresh is what a list holds as long as nothing changes: the table's token,
// its fields as they read, and the reader's day (a filter on "today"). A
// token Redis fails to give is none, which no token written matches.
func (l listing) fresh(ctx context.Context) string {
	token, _, _ := store.GetString(ctx, registry.TableRowsToken, []string{l.table.String()})
	defs := make([][3]string, len(l.fields))
	for i, f := range l.fields {
		defs[i] = [3]string{f.Id.String(), f.Type, f.Config}
	}
	return stamp(token, defs, l.who.now.In(l.who.zone).Format(time.DateOnly))
}

// runListed is a window after the first of a spec no walk answers: after is
// the one query's cursor, or a list's. Redis failing is the store's to log:
// a list it doesn't give is listed again.
func runListed(ctx context.Context, l listing, after query.Cursor, size int) ([]uuid.UUID, query.Cursor, error) {
	view, fresh := l.view(), l.fresh(ctx)
	if cursorKind(after) == "list" {
		c, err := readListCursor(after)
		if err != nil || c.view != view {
			return nil, "", ErrBadCursor
		}
		if c.fresh == fresh {
			// Read under the view this reader is asking for: a cursor
			// naming another's list can't reach it.
			b, _ := store.GetRange(ctx, registry.TableRowList, []string{view, c.key}, int64(c.at*idSize), int64((c.at+size+1)*idSize-1))
			if ids := listedIDs(b); len(ids) > 0 {
				return l.window(ctx, c, ids, size)
			}
		}
		after = c.after
	}
	return l.listFrom(ctx, after, view, fresh, size)
}

// listFrom lists the order after the one query's cursor, as many as
// listMax, and answers its first window.
func (l listing) listFrom(ctx context.Context, after query.Cursor, view, fresh string, size int) ([]uuid.UUID, query.Cursor, error) {
	found, err := oneQuery(ctx, l.table, l.cols, l.spec, l.where, l.a, after, listMax+1, l.who, l.unkeyed, false)
	if err != nil {
		return nil, "", err
	}
	c := listCursor{key: stamp(fresh, after), cut: len(found) > listMax, fresh: fresh, view: view, after: after}
	if c.cut {
		found = found[:listMax]
	}
	ids := make([]uuid.UUID, len(found))
	b := make([]byte, len(found)*idSize)
	for i, f := range found {
		ids[i] = f.ID
		copy(b[i*idSize:], f.ID[:])
	}
	if len(ids) > size || c.cut {
		// One not stored is listed again by the next window.
		_ = store.SetString(ctx, registry.TableRowList, []string{view, c.key}, string(b))
	}
	if len(ids) > size+1 {
		ids = ids[:size+1]
	}
	return l.window(ctx, c, ids, size)
}

// window is the window of the ids a list holds at c (one past it, when
// there are more), and the next window's cursor: after its last row still
// there, or where it began when every one went meanwhile.
func (l listing) window(ctx context.Context, c listCursor, ids []uuid.UUID, size int) ([]uuid.UUID, query.Cursor, error) {
	more := len(ids) > size
	if more {
		ids = ids[:size]
	}
	if !more && !c.cut {
		return ids, "", nil
	}
	after, found, err := oneQueryCursorAfter(ctx, l.table, l.cols, l.spec, ids[len(ids)-1:], l.who)
	if err == nil && !found {
		// The row shown last went meanwhile: after the last still there.
		after, found, err = oneQueryCursorAfter(ctx, l.table, l.cols, l.spec, ids, l.who)
	}
	if err != nil {
		return nil, "", err
	}
	if found {
		c.after = after
	}
	c.at += len(ids)
	return ids, c.cursor(), nil
}

// oneQueryAfter is a cursor as the one query reads it: a list's goes on
// from its row shown last (without Redis, which went away meanwhile).
func oneQueryAfter(after query.Cursor) (query.Cursor, error) {
	if cursorKind(after) != "list" {
		return after, nil
	}
	c, err := readListCursor(after)
	return c.after, err
}

// listedIDs is the ids a list's bytes hold.
func listedIDs(b string) []uuid.UUID {
	ids := make([]uuid.UUID, len(b)/idSize)
	for i := range ids {
		copy(ids[i][:], b[i*idSize:])
	}
	return ids
}
