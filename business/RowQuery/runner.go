package business

// The "table" scope's runner (tables A3): a spec of a table's rows answered
// on the server, for the reader, in windows of their view's order. Filters
// and sorts on stored cells and the row's own columns run as SQL (compile.go)
// over every row, with keyset cursors; a spec that reads a formula or a
// rollup, which nothing stores until A4, is answered by the evaluator over
// the rows one read can work out, and says so past them.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	computedStore "github.com/akashc777/OneCamp/business/ComputedStore"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// CountCap is the most rows a total counts: past it, a total says "100,000+".
const CountCap = 100000

// NotCounted is a window's Total when it isn't counted: a window after the
// first, and a first window that didn't ask (WithCount). Counting a set reads
// all of it, so a reader asks for it beside its first window, not in it: the
// window answers in the time an index range takes (perf-mobile's 20 ms at
// 100,000 rows) and the count in its own (tables A3, T5).
const NotCounted = -1

type countKey struct{}

// WithCount has a first window count its set (Total).
func WithCount(ctx context.Context) context.Context {
	return context.WithValue(ctx, countKey{}, true)
}

func counting(ctx context.Context) bool {
	b, _ := ctx.Value(countKey{}).(bool)
	return b
}

// MaxComputedScan is the most rows a spec reading a formula or a rollup is
// answered over, until A4 stores their values.
const MaxComputedScan = 10000

// ErrTooManyToWorkOut is a spec reading formulas over more rows than one
// read works out.
// Its words cover each use (a filter, a sort, a group or a summary), as
// a reader sees them (airtable-03).
var ErrTooManyToWorkOut = errors.New("row query: this table has too many rows to work out a formula or a rollup over all of them yet")

// StillWorkingOut is a spec reading a formula or a rollup the Computed Store
// keeps, over more rows than one read works out, while the store is still
// filling it (a new field, or one whose settings changed): not a refusal but
// how far the fill has got, so the reader's view says so and asks again
// (airtable-03b). It is ErrTooManyToWorkOut to anything that only knows that.
type StillWorkingOut struct {
	Field       string
	Done, Total int64
}

func (e *StillWorkingOut) Error() string {
	if e.Total <= 0 {
		return fmt.Sprintf("row query: %s is being worked out over every row: starting", e.Field)
	}
	return fmt.Sprintf("row query: %s is being worked out over %s rows: %d%% done", e.Field, thousands(e.Total), e.Done*100/e.Total)
}

// Is makes a fill still running ErrTooManyToWorkOut to errors.Is.
func (e *StillWorkingOut) Is(target error) bool { return target == ErrTooManyToWorkOut }

// stillWorkingOut is how far the store has got filling the first of fields
// it keeps but hasn't filled as their settings are now, the least far along
// (a fill sets their progress together), or nil when none waits for a fill:
// then a read past what one read works out is refused (airtable-03b). A
// field waiting for its fill wakes the sweep that fills it, once for each of
// the field's changes, so its reader waits for the fill and not for the
// sweep's next tick, up to a minute on (V-114 (b)).
func stillWorkingOut(ctx context.Context, fields []*model.Field) *StillWorkingOut {
	ids := make([]uuid.UUID, 0, len(fields))
	byID := make(map[uuid.UUID]*model.Field, len(fields))
	for _, f := range fields {
		ids, byID[f.Id] = append(ids, f.Id), f
	}
	states, err := model.ComputedStates(ctx, ids)
	if err != nil {
		return nil
	}
	var out *StillWorkingOut
	var changes []string
	for _, id := range ids {
		s, ok := states[id]
		if !ok || s.FilledGen == s.Gen {
			continue
		}
		changes = append(changes, fmt.Sprintf("%s@%d", id, s.Gen))
		w := &StillWorkingOut{Field: byID[id].Name, Done: s.FillDone, Total: s.FillTotal}
		if out == nil || w.Total <= 0 || (out.Total > 0 && w.Done*out.Total < out.Done*w.Total) {
			out = w
		}
	}
	if len(changes) > 0 {
		sweepBusiness.Jobs.Wake(computedStore.SweepRecompute, changes...)
	}
	return out
}

// thousands is n with its thousands marked: 100000 is "100,000".
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// ErrNotFound is a table the reader can't see, or that isn't there: the same
// to them.
var ErrNotFound = errors.New("row query: that table isn't there")

// ErrBadCursor is a cursor from another query, or written by hand.
var ErrBadCursor = errors.New("row query: that page marker isn't this query's")

type zoneKey struct{}

// WithZone has a query read dates and "today" in the reader's zone.
func WithZone(ctx context.Context, loc *time.Location) context.Context {
	return context.WithValue(ctx, zoneKey{}, loc)
}

func zoneOf(ctx context.Context) *time.Location {
	if loc, ok := ctx.Value(zoneKey{}).(*time.Location); ok && loc != nil {
		return loc
	}
	return time.UTC
}

// Worked is a page of a table's rows with their formulas and rollups worked
// out for the reader, as business/DataTable reads them, and what each gives.
type Worked func(ctx context.Context, who tableAccess.Who, table uuid.UUID, fields []*model.Field, max int) (rows []*model.Row, more bool, kinds map[string]string, err error)

// WorkedOut is business/DataTable's, registered at init: RowQuery can't
// import it (DataTable imports RowQuery).
var WorkedOut Worked

// Runner answers specs whose scope is a table.
type Runner struct{}

func init() {
	query.Queries.Register(query.ScopeTable, Runner{})
}

// whoOf is the asker as tables' rules see them.
func whoOf(s accessBusiness.Subject) tableAccess.Who {
	user, _ := uuid.Parse(s.UserUUID)
	agent, _ := uuid.Parse(s.AgentUUID)
	return tableAccess.Who{User: user, Admin: s.Admin, Agent: agent}
}

// Run answers one window of a spec.
func (Runner) Run(ctx context.Context, who accessBusiness.Subject, spec query.Spec, page query.Page) (query.Result, error) {
	if err := spec.Validate(); err != nil {
		return query.Result{}, err
	}
	table, err := uuid.Parse(spec.Scope.ID)
	if err != nil {
		return query.Result{}, ErrNotFound
	}
	d, err := tableAccess.Tables.Resolve(ctx, whoOf(who), table)
	if err != nil || !d.View {
		return query.Result{}, ErrNotFound
	}
	all, err := model.ListFields(ctx, table)
	if err != nil {
		return query.Result{}, err
	}
	var fields []*model.Field
	for _, f := range all {
		if f.DeletedAt == nil {
			fields = append(fields, f)
		}
	}
	cols := columns(fields)
	tw := whoOf(who)
	asked := asker{user: tw.User, admin: tw.Admin, agent: tw.Agent, now: time.Now(), zone: zoneOf(ctx)}
	size := page.Size
	if size <= 0 || size > query.MaxPage {
		size = 100
	}
	// The index answers a table it is whole for, as far as its entries' version
	// holds what the spec reads; a formula or a rollup, a table still being
	// indexed, or a sort on a text or a choice before its entries hold their
	// keys, the evaluator, over what one read works out.
	// A formula or a rollup the Computed Store keeps answers along the index
	// too, but only a reader its gate serves (keptCols): every other reader
	// is answered by their own read, worked out as them.
	whole, version, err := model.CellIndexState(ctx, table)
	if err != nil {
		return query.Result{}, err
	}
	served := cols
	if read := computedRead(spec, cols); whole && len(read) > 0 {
		if kept := keptCols(ctx, tw, table, read, cols); kept != nil {
			served = kept
		}
	}
	if !whole || readsComputed(spec, served) || (version < sortKeysSince && sortsByKey(spec, served)) {
		return runWorked(ctx, table, fields, cols, spec, page.After, size, asked)
	}
	return runSQL(ctx, table, fields, served, spec, page.After, size, asked)
}

// sortsByKey is whether a spec sorts or groups by a text or a choice: by the
// entries' s, their natural sort key.
func sortsByKey(spec query.Spec, cols map[string]*column) bool {
	for _, s := range specOrder(spec) {
		if c := cols[s.Field]; c != nil && c.col == colNone && (c.family == famText || c.family == famChoice) {
			return true
		}
	}
	return false
}

// Answers is what Row Query answers for a table from its cell index, as the
// table's bundle tells a reader (tables T5c): Indexed, filters and sorts at
// any depth of its rows; SortsText, a sort on a text or a choice too, in the
// natural key's order (sortKey.go). A reader asks the server for windows of
// a view only what it answers, and keeps the rest in the browser.
type Answers struct {
	Indexed   bool `json:"indexed"`
	SortsText bool `json:"sorts_text"`
}

// AnswersFor is what Row Query answers for table now.
func AnswersFor(ctx context.Context, table uuid.UUID) (Answers, error) {
	whole, version, err := model.CellIndexState(ctx, table)
	return Answers{Indexed: whole, SortsText: whole && version >= sortKeysSince}, err
}

// readsComputed is whether a spec filters, sorts, groups or sums up by a
// formula or a rollup the index doesn't answer.
func readsComputed(spec query.Spec, cols map[string]*column) bool {
	return len(computedRead(spec, cols)) > 0
}

// searchSQL is a list's own search: every word in some cell, ignoring case.
func searchSQL(search string, a *sqlArgs) string {
	var parts []string
	for _, w := range strings.Fields(strings.ToLower(search)) {
		parts = append(parts, `EXISTS (SELECT 1 FROM jsonb_each_text(r."values") kv WHERE strpos(lower(kv.value), `+a.add(w)+`::text) > 0)`)
	}
	if len(parts) == 0 {
		return "TRUE"
	}
	return strings.Join(parts, " AND ")
}

// cursorOf writes a window's last row's keys as an opaque marker.
func cursorOf(kind string, keys []interface{}) query.Cursor {
	vals := make([]interface{}, len(keys))
	for i, k := range keys {
		if t, ok := k.(time.Time); ok {
			vals[i] = t.UTC().Format(time.RFC3339Nano)
		} else {
			vals[i] = k
		}
	}
	b, _ := json.Marshal(map[string]interface{}{"k": kind, "v": vals})
	return query.Cursor(base64.RawURLEncoding.EncodeToString(b))
}

// WalkBudget is how long a window's walks may take before the window is the
// one query's (runSQL). A seam for tests.
var WalkBudget = 150 * time.Millisecond

// summarize reads a first window's summaries and groups
// (model.SummarizeRowsWhere). A seam for tests.
var summarize = model.SummarizeRowsWhere

// SummaryBudget is how long a first window's summaries and groups may take;
// past it the window and its count stand without them. Zero is no budget. A
// seam for tests.
var SummaryBudget = 2 * time.Second

// cursorKind is the kind a cursor names ("walk", "sql"); "" for none it reads.
func cursorKind(c query.Cursor) string {
	b, err := base64.RawURLEncoding.DecodeString(string(c))
	if err != nil {
		return ""
	}
	var m struct {
		K string `json:"k"`
	}
	_ = json.Unmarshal(b, &m)
	return m.K
}

func readCursor(c query.Cursor, kind string, n int) ([]interface{}, error) {
	b, err := base64.RawURLEncoding.DecodeString(string(c))
	if err != nil {
		return nil, ErrBadCursor
	}
	var m struct {
		K string        `json:"k"`
		V []interface{} `json:"v"`
	}
	if json.Unmarshal(b, &m) != nil || m.K != kind || (n >= 0 && len(m.V) != n) {
		return nil, ErrBadCursor
	}
	return m.V, nil
}

func runSQL(ctx context.Context, table uuid.UUID, fields []*model.Field, cols map[string]*column, spec query.Spec, after query.Cursor, size int, who asker) (query.Result, error) {
	a := &sqlArgs{base: 1}
	where, err := whereOf(spec.Filter, cols, who, a)
	if err != nil {
		return query.Result{}, err
	}
	where = "(" + where + ") AND (" + searchSQL(spec.Search, a) + ")"
	countArgs := append([]interface{}{}, a.vals...)
	if err := checkGroups(spec.Groups, cols, nil); err != nil {
		return query.Result{}, err
	}
	// The windows leave out the rows of the groups the reader collapsed; the
	// count and the groups' counts never do.
	shown := where
	if hide := collapsedSQL(spec.Groups, cols, who, a); hide != "" {
		shown += " AND " + hide
	}
	walks, unknown, err := walksOf(specOrder(spec), spec.Filter, cols)
	if err != nil {
		return query.Result{}, err
	}
	if unknown != nil {
		// A label no option has sorts after them all, by itself: the one
		// query sorts a choice that has any.
		some, err := model.FieldHasPlaceAbove(ctx, unknown.field.Id, float64(len(unknown.options)))
		if err != nil {
			return query.Result{}, err
		}
		if some {
			walks = nil
		}
	}
	unkeyed, err := model.HasUnkeyedRows(ctx, table)
	if err != nil {
		return query.Result{}, err
	}
	// The set's summaries and its groups, for a first window that counts,
	// beside it as the count is (a reader asks for them in a request of its
	// own, so its rows never wait on them): over every row the spec keeps,
	// never a capped count of them, within SummaryBudget.
	var sums query.Summaries
	var groups []query.GroupCount
	var summed chan error
	if after == "" && counting(ctx) && (len(spec.Summaries) > 0 || len(spec.Groups) > 0) {
		if err := checkSummaries(spec.Summaries, cols, nil); err != nil {
			return query.Result{}, err
		}
		sa := &sqlArgs{base: a.base, vals: append([]interface{}{}, countArgs...)}
		read := aggregateSQL(spec, cols, who, sa)
		summed = make(chan error, 1)
		helpers.GoSafeNamed("RowQuery.summaries", func() {
			defer helpers.RecoverToErr("RowQuery.summaries", summed)
			err := summarize(ctx, table, strings.Join(sa.joins, " "), read.selects, where, read.grouped, sa.vals, SummaryBudget, read.scan)
			if err == nil {
				sums, groups, err = read.result()
			}
			summed <- err
		})
	}
	// Counted for a first window that asks, beside it: a reader keeps it, and
	// a count of every window would read the whole set again each time.
	total := NotCounted
	var counted chan error
	if after == "" && counting(ctx) {
		counted = make(chan error, 1)
		helpers.GoSafeNamed("RowQuery.count", func() {
			// A panic is the count's error, so the window never waits for a
			// count that isn't coming.
			defer helpers.RecoverToErr("RowQuery.count", counted)
			n, err := model.CountRowsWhere(ctx, table, where, countArgs, CountCap)
			total = n
			counted <- err
		})
	}
	var ids []uuid.UUID
	var next query.Cursor
	// A listing that began as the one query goes on as it, or as its list
	// (its cursor says so).
	if k := cursorKind(after); k == "sql" || k == "list" {
		walks = nil
	}
	if walks != nil && !unkeyed {
		// A window walks under a budget: a filter few rows meet makes a walk
		// read most of its index for them, where the one query sorts the few.
		// Past the budget the listing is the one query's, from the walk's
		// last row on (the two orders are one: the evaluator's).
		var found []walked
		walkArgs := append([]interface{}{}, a.vals...)
		found, err = runWalks(ctx, table, walks, shown, a, spec.Filter, who, after, size, WalkBudget)
		if errors.Is(err, model.ErrWalkTooLong) {
			a.vals = walkArgs
			walks = nil
			found, err = nil, nil
			if after != "" {
				if after, err = oneQueryCursorAt(ctx, table, cols, spec, after, who); err != nil {
					return query.Result{}, err
				}
			}
		}
		for i, f := range found {
			if i == size {
				last := found[size-1]
				next = cursorOf("walk", append([]interface{}{last.walk}, last.keys...))
				break
			}
			ids = append(ids, f.id)
		}
	}
	if walks == nil || unkeyed {
		if after != "" && store.IsAvailable() {
			ids, next, err = runListed(ctx, listing{table: table, fields: fields, cols: cols, spec: spec, where: shown, a: a, who: who, unkeyed: unkeyed}, after, size)
		} else if after, err = oneQueryAfter(after); err == nil {
			ids, next, err = runOneQuery(ctx, table, cols, spec, shown, a, after, size, who, unkeyed)
		}
	}
	if counted != nil {
		if cerr := <-counted; err == nil {
			err = cerr
		}
	}
	if summed != nil {
		// A read that fails, or runs past its budget, leaves the summaries and
		// the groups out, never the window, its order or its count: the bar
		// keeps what it shows. More groups than the counts answer is refused.
		if serr := <-summed; errors.Is(serr, ErrCantQuery) {
			if err == nil {
				err = serr
			}
		} else if serr != nil {
			helpers.LogWarnWithContext(ctx, "RowQuery: table %s: summaries and groups left out: %v", table, serr)
			sums, groups = nil, nil
		}
	}
	if err != nil {
		return query.Result{}, err
	}
	res := query.Result{IDs: make([]string, 0, len(ids)), Total: total, Next: next, Summaries: sums, Groups: groups}
	for _, id := range ids {
		res.IDs = append(res.IDs, id.String())
	}
	return res, nil
}

// oneQueryCursorAt is a walk's cursor as the one query's: the one query's
// keys of the row the walk stopped after (a walk's keys end with its id).
func oneQueryCursorAt(ctx context.Context, table uuid.UUID, cols map[string]*column, spec query.Spec, walkCursor query.Cursor, who asker) (query.Cursor, error) {
	at, err := readCursor(walkCursor, "walk", -1)
	if err != nil || len(at) == 0 {
		return "", ErrBadCursor
	}
	s, _ := at[len(at)-1].(string)
	row, err := uuid.Parse(s)
	if err != nil {
		return "", ErrBadCursor
	}
	c, found, err := oneQueryCursorAfter(ctx, table, cols, spec, []uuid.UUID{row}, who)
	if err == nil && !found {
		err = ErrBadCursor
	}
	return c, err
}

// oneQueryCursorAfter is the one query's cursor after the last of rows in
// the spec's order: its keys there. found is false when every one is gone.
func oneQueryCursorAfter(ctx context.Context, table uuid.UUID, cols map[string]*column, spec query.Spec, rows []uuid.UUID, who asker) (query.Cursor, bool, error) {
	a := &sqlArgs{base: 1}
	keys, err := sortKeysOf(specOrder(spec), cols, who, a)
	if err != nil {
		return "", false, err
	}
	exprs := make([]string, len(keys))
	for i, k := range keys {
		exprs[i] = k.expr
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.String()
	}
	where := "r.id = ANY(" + a.add(ids) + "::uuid[])"
	found, err := model.QueryRowWindow(ctx, table, strings.Join(a.joins, " "), where, exprs, orderByLast(keys), a.vals, 1)
	if err != nil || len(found) == 0 {
		return "", false, err
	}
	return cursorOf("sql", found[0].Keys), true, nil
}

// oneQuery is compile.go's one query: every sort in SQL, up to limit of the
// rows that meet where, after the one query's cursor ("" from the first),
// with each row's keys when keyed.
func oneQuery(ctx context.Context, table uuid.UUID, cols map[string]*column, spec query.Spec, where string, a *sqlArgs, after query.Cursor, limit int, who asker, unkeyed, keyed bool) ([]model.KeyedRow, error) {
	keys, err := sortKeysOf(specOrder(spec), cols, who, a)
	if err != nil {
		return nil, err
	}
	if after != "" {
		at, err := readCursor(after, "sql", len(keys))
		if err != nil {
			return nil, err
		}
		where += " AND (" + afterSQL(keys, at, a, unkeyed) + ")"
	}
	var exprs []string
	if keyed {
		exprs = make([]string, len(keys))
		for i, k := range keys {
			exprs[i] = k.expr
		}
	}
	return model.QueryRowWindow(ctx, table, strings.Join(a.joins, " "), where, exprs, orderBy(keys), a.vals, limit)
}

// runOneQuery is a window as the one query.
func runOneQuery(ctx context.Context, table uuid.UUID, cols map[string]*column, spec query.Spec, where string, a *sqlArgs, after query.Cursor, size int, who asker, unkeyed bool) ([]uuid.UUID, query.Cursor, error) {
	found, err := oneQuery(ctx, table, cols, spec, where, a, after, size+1, who, unkeyed, true)
	if err != nil {
		return nil, "", err
	}
	var ids []uuid.UUID
	var next query.Cursor
	for i, f := range found {
		if i == size {
			next = cursorOf("sql", found[size-1].Keys)
			break
		}
		ids = append(ids, f.ID)
	}
	return ids, next, nil
}

// evalRowOf is a stored row as the evaluator reads it.
func evalRowOf(r *model.Row) *evalRow {
	er := rowOf(r)
	_ = json.Unmarshal([]byte(r.Values), &er.values)
	if er.values == nil {
		er.values = map[string]interface{}{}
	}
	return er
}

// rowOf is what the evaluator reads of a row itself, without its cells: its
// place, its number, when it was made and changed, and by whom.
func rowOf(r *model.Row) *evalRow {
	er := &evalRow{id: r.Id, positionKey: r.PositionKey, rowNo: r.RowNo, created: r.CreatedAt, updated: r.UpdatedAt}
	if r.CreatedBy != nil {
		er.createdBy = *r.CreatedBy
	}
	if r.UpdatedBy != nil {
		er.updatedBy = *r.UpdatedBy
	}
	return er
}

// kindsOf is what each formula and rollup gives ("number", "date",
// "checkbox" or text, by id) as the families they compare as.
func kindsOf(results map[string]string) computedKinds {
	kinds := make(computedKinds, len(results))
	for id, res := range results {
		kinds[id] = kindOfResult(res)
	}
	return kinds
}

// kindOfResult is the family a formula's or a rollup's result compares as.
func kindOfResult(result string) family {
	switch result {
	case "number":
		return famNumber
	case "date":
		return famDate
	case "checkbox":
		return famCheckbox
	}
	return famText
}

// searchMatches is searchSQL in Go: every word in some cell's text.
func searchMatches(search string, r *evalRow) bool {
	for _, w := range strings.Fields(strings.ToLower(search)) {
		found := false
		for _, v := range r.values {
			if t, ok := textOf(v); ok && strings.Contains(strings.ToLower(t), w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func runWorked(ctx context.Context, table uuid.UUID, fields []*model.Field, cols map[string]*column, spec query.Spec, after query.Cursor, size int, who asker) (query.Result, error) {
	if WorkedOut == nil {
		return query.Result{}, cantQuery("formulas can't be read here")
	}
	// A view asks again every few seconds while a fill runs (airtable-03b),
	// so a fill under way over more rows than one read works out says how far
	// before any work, never after working out 10,000 rows to find more
	// (CR-95 M1). A fill not yet counted is found past the limit as before.
	if w := stillWorkingOut(ctx, fields); w != nil && w.Total > MaxComputedScan {
		return query.Result{}, w
	}
	rows, more, results, err := WorkedOut(ctx, tableAccess.Who{User: who.user, Admin: who.admin, Agent: who.agent}, table, fields, MaxComputedScan)
	if err != nil {
		return query.Result{}, err
	}
	if more {
		if w := stillWorkingOut(ctx, fields); w != nil {
			return query.Result{}, w
		}
		return query.Result{}, ErrTooManyToWorkOut
	}
	read := make([]*evalRow, len(rows))
	for i, r := range rows {
		read[i] = evalRowOf(r)
	}
	ev, err := evaluate(spec, cols, kindsOf(results), read, who, after == "" && counting(ctx))
	if err != nil {
		return query.Result{}, err
	}
	from := 0
	if after != "" {
		at, err := readCursor(after, "worked", 1)
		if err != nil {
			return query.Result{}, err
		}
		n, _ := at[0].(float64)
		from = int(n)
	}
	res := query.Result{IDs: []string{}, Total: len(ev.kept), Summaries: ev.summaries, Groups: ev.groups}
	if after != "" {
		res.Total = NotCounted
	}
	for i := from; i < len(ev.shown) && i < from+size; i++ {
		res.IDs = append(res.IDs, ev.shown[i].id.String())
	}
	if from+size < len(ev.shown) {
		res.Next = cursorOf("worked", []interface{}{float64(from + size)})
	}
	return res, nil
}

// evaluated is a spec answered by the evaluator over the rows read: those
// it keeps, in its order; those its windows show (none of a collapsed
// group's); and, when asked, the set's summaries and its groups.
type evaluated struct {
	kept, shown []*evalRow
	summaries   query.Summaries
	groups      []query.GroupCount
}

// evaluate answers spec over rows by the evaluator, the summaries and the
// groups too when aggregate asks (a first window that counts).
func evaluate(spec query.Spec, cols map[string]*column, kinds computedKinds, rows []*evalRow, who asker, aggregate bool) (evaluated, error) {
	var ev evaluated
	if err := checkGroups(spec.Groups, cols, kinds); err != nil {
		return ev, err
	}
	for _, r := range rows {
		ok, err := matches(spec.Filter, r, cols, kinds, who)
		if err != nil {
			return ev, err
		}
		if ok && searchMatches(spec.Search, r) {
			ev.kept = append(ev.kept, r)
		}
	}
	if err := sortRows(ev.kept, specOrder(spec), cols, kinds, who); err != nil {
		return ev, err
	}
	ev.shown = ev.kept
	var paths [][]string // each kept row's keys, when the spec groups
	if len(spec.Groups) > 0 {
		collapsed := collapsedOf(spec.Groups, cols, kinds)
		paths, ev.shown = make([][]string, len(ev.kept)), nil
		for i, r := range ev.kept {
			paths[i] = pathOf(spec.Groups, cols, kinds, r, who.zone)
			if !hidden(paths[i], collapsed) {
				ev.shown = append(ev.shown, r)
			}
		}
	}
	if !aggregate {
		return ev, nil
	}
	if err := checkSummaries(spec.Summaries, cols, kinds); err != nil {
		return ev, err
	}
	var err error
	if len(spec.Summaries) > 0 {
		if ev.summaries, err = foldSummaries(spec.Summaries, cols, kinds, ev.kept); err != nil {
			return ev, err
		}
	}
	if len(spec.Groups) > 0 {
		ev.groups, err = foldGroups(spec.Summaries, cols, kinds, ev.kept, paths)
	}
	return ev, err
}
