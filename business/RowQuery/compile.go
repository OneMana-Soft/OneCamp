package business

// A spec as SQL over a table's rows and its cell index (tables A3): its
// filter as conditions on each cell's entry (cellIndex.go), which the index
// on (table, field, value, row) answers; its sorts on the entries joined, with
// every empty cell last and the table's own order breaking ties; a cursor as
// a keyset condition, never an OFFSET. It means what eval.go means, row for
// row (the property test holds it), because the entries are worked out by
// the same families the evaluator reads.
//
// Everything the asker writes is a parameter. The row's own columns (its
// number, when it was made and changed, and by whom) and links to a table's
// rows are read from the row and from data_table_links.

import (
	"strconv"
	"strings"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
)

// sqlArgs collects a query's parameters; the first base are the caller's.
type sqlArgs struct {
	base int
	vals []interface{}
	// joins are the sorts' entries, joined to r.
	joins []string
	// on, when set, is an entry already joined (a walk's s): a condition on
	// its field reads that entry, not a probe of its own (onEntry).
	on string
}

func (a *sqlArgs) add(v interface{}) string {
	a.vals = append(a.vals, v)
	return "$" + strconv.Itoa(a.base+len(a.vals))
}

// entry is "the cell has an entry meeting cond" (cond on ci; "" for any).
func (a *sqlArgs) entry(c *column, cond string) string {
	if cond == "" {
		// A cell with a value: a walked column's empty cells have an entry
		// too (cellIndex.go cellEntry), with nothing in it.
		cond = valued
	}
	if a.on != "" {
		// The row's one entry of the field is there already (by the primary
		// key, (row, field), a probe's EXISTS would find it alone): its
		// condition is the entry's own, and NOT the same either way.
		return "(" + strings.ReplaceAll(cond, "ci.", a.on+".") + ")"
	}
	// By (row, field): a probe reads the primary key, and a set the planner
	// hashes reads the field's own range of an index the field leads.
	q := `EXISTS (SELECT 1 FROM data_table_cell_index ci WHERE ci.row_id = r.id AND ci.field_id = ` + a.add(c.key) + `::uuid`
	if cond != "" {
		q += ` AND (` + cond + `)`
	}
	return q + `)`
}

func not(s string) string { return "NOT " + s }

// negatedIf is s, or its NOT: a "doesn't" condition, which takes in the
// cells with no entry, as the browser's filters do.
func negatedIf(negate bool, s string) string {
	if negate {
		return not(s)
	}
	return s
}

// valued is an entry holding a value: an empty cell's entry holds none.
const valued = `ci.n IS NOT NULL OR ci.t IS NOT NULL OR ci.d IS NOT NULL OR ci.b IS NOT NULL OR ci.a IS NOT NULL`

// emptyEntry is s, a walk's entry, holding no value (the index's empty part).
const emptyEntry = `s.n IS NULL AND s.t IS NULL AND s.d IS NULL AND s.b IS NULL AND s.a IS NULL`

// idsExpr is a row column's person, or the rows a link links to, as an
// array of ids; an empty array for none. Who last changed a row is whoever
// made it until someone does (idsOf).
func (a *sqlArgs) idsExpr(c *column) string {
	switch c.col {
	case colCreatedBy:
		return `CASE WHEN r.created_by IS NULL THEN '{}'::text[] ELSE ARRAY[r.created_by::text] END`
	case colUpdatedBy:
		return `CASE WHEN COALESCE(r.updated_by, r.created_by) IS NULL THEN '{}'::text[] ELSE ARRAY[COALESCE(r.updated_by, r.created_by)::text] END`
	}
	field := a.add(c.linkField.String())
	if c.reverse {
		return `COALESCE((SELECT array_agg(l.from_row::text) FROM data_table_links l WHERE l.field_id = ` + field + `::uuid AND l.to_row = r.id), '{}')`
	}
	return `COALESCE((SELECT array_agg(l.to_row::text) FROM data_table_links l WHERE l.field_id = ` + field + `::uuid AND l.from_row = r.id), '{}')`
}

// whereOf is a filter as a condition on r, never NULL.
func whereOf(f *query.Filter, cols map[string]*column, who asker, a *sqlArgs) (string, error) {
	if f == nil {
		return "TRUE", nil
	}
	switch f.Op {
	case query.OpAnd, query.OpOr:
		if len(f.Filters) == 0 {
			if f.Op == query.OpAnd {
				return "TRUE", nil
			}
			return "FALSE", nil
		}
		parts := make([]string, 0, len(f.Filters))
		for i := range f.Filters {
			p, err := whereOf(&f.Filters[i], cols, who, a)
			if err != nil {
				return "", err
			}
			parts = append(parts, "("+p+")")
		}
		join := " AND "
		if f.Op == query.OpOr {
			join = " OR "
		}
		return strings.Join(parts, join), nil
	case query.OpNot:
		p, err := whereOf(&f.Filters[0], cols, who, a)
		if err != nil {
			return "", err
		}
		return "NOT (" + p + ")", nil
	}
	c := cols[f.Field]
	if c == nil {
		return "", errNoField(f.Field)
	}
	if c.family == famComputed {
		return "", cantQuery("%s is worked out on each read, so the server can't filter by it yet", c.field.Name)
	}
	sql, err := conditionSQL(c, f.Operator, f.Values, who, a)
	if err != nil {
		return "", err
	}
	// Never NULL, so NOT and OR mean what they say: a cell that isn't
	// there doesn't meet a condition, and its NOT does. An EXISTS is never
	// NULL already, and left bare the planner can join it rather than probe
	// each row.
	if loneExists(sql) {
		return sql, nil
	}
	return "COALESCE((" + sql + "), FALSE)", nil
}

// loneExists is whether sql is one EXISTS (…) or NOT EXISTS (…) and nothing
// else: its first bracket closes at its end. Values are parameters, so no
// bracket is ever inside a quoted string.
func loneExists(sql string) bool {
	rest := strings.TrimPrefix(sql, "NOT ")
	if !strings.HasPrefix(rest, "EXISTS (") {
		return false
	}
	depth := 0
	for i, ch := range rest[len("EXISTS "):] {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i == len(rest)-len("EXISTS ")-1
			}
		}
	}
	return false
}

func conditionSQL(c *column, op string, vals []query.Value, who asker, a *sqlArgs) (string, error) {
	switch c.family {
	case famText:
		return textSQL(c, op, vals, a)
	case famNumber:
		return numberSQL(c, op, vals, a)
	case famDate:
		return dateSQL(c, op, vals, who, a)
	case famCheckbox:
		switch op {
		case query.OperatorChecked:
			return a.entry(c, ""), nil
		case query.OperatorUnchecked:
			return not(a.entry(c, "")), nil
		}
	case famChoice:
		return labelSQL(c, op, wantLabels(c, op, vals), a)
	case famMulti:
		return setSQL(c, op, wantLabels(c, op, vals), a, true)
	case famPeople, famRefs, famRowLink:
		want := make([]string, 0, len(vals))
		for _, v := range vals {
			want = append(want, valueID(v, who))
		}
		if c.col != colNone || c.family == famRowLink {
			return idsSQL(a.idsExpr(c), op, want, a)
		}
		return setSQL(c, op, want, a, false)
	}
	return "", cantQuery("%s can't be compared with %s", c.field.Name, op)
}

// wantLabels is a choice's values as the labels an entry holds.
func wantLabels(c *column, op string, vals []query.Value) []string {
	want := []string{}
	if op == query.OperatorStatusGroupIs {
		for _, v := range vals {
			for _, l := range c.group[strings.TrimSpace(v.Text)] {
				want = append(want, strings.ToLower(strings.TrimSpace(l)))
			}
		}
		return want
	}
	for _, v := range vals {
		want = append(want, strings.ToLower(strings.TrimSpace(valueLabel(c, v))))
	}
	return want
}

// likePrefix is text as a LIKE pattern's start, its wildcards taken as written.
func likePrefix(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}

func textSQL(c *column, op string, vals []query.Value, a *sqlArgs) (string, error) {
	want := make([]string, len(vals))
	for i, v := range vals {
		want[i] = strings.ToLower(strings.TrimSpace(v.Text))
	}
	anyOf := func(test func(w string) string) string {
		parts := make([]string, len(want))
		for i, w := range want {
			parts[i] = test(w)
		}
		return strings.Join(parts, " OR ")
	}
	switch op {
	case query.OperatorEmpty:
		return not(a.entry(c, "")), nil
	case query.OperatorNotEmpty:
		return a.entry(c, ""), nil
	case query.OperatorIs, query.OperatorIn:
		return a.entry(c, `ci.t = ANY(`+a.add(want)+`::text[])`), nil
	case query.OperatorIsNot, query.OperatorNotIn:
		return not(a.entry(c, `ci.t = ANY(`+a.add(want)+`::text[])`)), nil
	case query.OperatorContains, query.OperatorNotContains:
		return negatedIf(op == query.OperatorNotContains, a.entry(c, anyOf(func(w string) string { return `strpos(ci.t, ` + a.add(w) + `::text) > 0` }))), nil
	case query.OperatorStartsWith:
		return a.entry(c, anyOf(func(w string) string { return `ci.t LIKE ` + a.add(likePrefix(w)) + `::text ESCAPE '\'` })), nil
	case query.OperatorEndsWith:
		return a.entry(c, anyOf(func(w string) string {
			p := a.add(w) + `::text`
			return `right(ci.t, length(` + p + `)) = ` + p
		})), nil
	}
	return "", cantQuery("text can't be compared with %s", op)
}

func numberSQL(c *column, op string, vals []query.Value, a *sqlArgs) (string, error) {
	nums := make([]string, len(vals))
	for i, v := range vals {
		f, err := valueNumber(v)
		if err != nil {
			return "", err
		}
		nums[i] = strconv.FormatFloat(f, 'f', -1, 64)
	}
	// The row's number is its own column; any other number an entry's n.
	test := func(cond func(e string) string) string {
		if c.col == colRowNo {
			return `r.row_no > 0 AND ` + cond(`r.row_no::numeric`)
		}
		return a.entry(c, cond(`ci.n`))
	}
	present := func() string {
		if c.col == colRowNo {
			return `r.row_no > 0`
		}
		return a.entry(c, "")
	}
	one := func(i int) string { return a.add(nums[i]) + `::numeric` }
	switch op {
	case query.OperatorEmpty:
		return not(present()), nil
	case query.OperatorNotEmpty:
		return present(), nil
	case query.OperatorIs, query.OperatorIn:
		return test(func(e string) string { return e + ` = ANY(` + a.add(nums) + `::numeric[])` }), nil
	case query.OperatorIsNot, query.OperatorNotIn:
		return not("(" + test(func(e string) string { return e + ` = ANY(` + a.add(nums) + `::numeric[])` }) + ")"), nil
	case query.OperatorGt:
		return test(func(e string) string { return e + ` > ` + one(0) }), nil
	case query.OperatorGte:
		return test(func(e string) string { return e + ` >= ` + one(0) }), nil
	case query.OperatorLt:
		return test(func(e string) string { return e + ` < ` + one(0) }), nil
	case query.OperatorLte:
		return test(func(e string) string { return e + ` <= ` + one(0) }), nil
	case query.OperatorBetween:
		return test(func(e string) string { return e + ` BETWEEN ` + one(0) + ` AND ` + one(1) }), nil
	}
	return "", cantQuery("a number can't be compared with %s", op)
}

func dateSQL(c *column, op string, vals []query.Value, who asker, a *sqlArgs) (string, error) {
	if c.col != colNone {
		col := `r.created_at`
		if c.col == colUpdatedAt {
			col = `r.updated_at`
		}
		switch op {
		case query.OperatorEmpty:
			return col + ` IS NULL`, nil
		case query.OperatorNotEmpty:
			return col + ` IS NOT NULL`, nil
		}
		from, to, err := dateWindow(op, vals, who)
		if err != nil {
			return "", err
		}
		parts := []string{col + ` IS NOT NULL`}
		if !from.IsZero() {
			parts = append(parts, col+` >= `+a.add(from)+`::timestamptz`)
		}
		if !to.IsZero() {
			parts = append(parts, col+` < `+a.add(to)+`::timestamptz`)
		}
		return strings.Join(parts, " AND "), nil
	}
	switch op {
	case query.OperatorEmpty:
		return not(a.entry(c, "")), nil
	case query.OperatorNotEmpty:
		return a.entry(c, ""), nil
	}
	from, to, err := dateWindow(op, vals, who)
	if err != nil {
		return "", err
	}
	// A day's entry is its date, a moment's its instant. The window's ends
	// are midnights where the reader is, so a day is in it when its date is
	// between theirs.
	days := []string{`ci.t IS NOT NULL`}
	moments := []string{`ci.t IS NULL`, `ci.d IS NOT NULL`} // a day has d too, its sort key
	if !from.IsZero() {
		days = append(days, `ci.t >= `+a.add(from.In(who.zone).Format(dayLayout))+`::text`)
		moments = append(moments, `ci.d >= `+a.add(from)+`::timestamptz`)
	}
	if !to.IsZero() {
		days = append(days, `ci.t < `+a.add(to.In(who.zone).Format(dayLayout))+`::text`)
		moments = append(moments, `ci.d < `+a.add(to)+`::timestamptz`)
	}
	return a.entry(c, `(`+strings.Join(days, " AND ")+`) OR (`+strings.Join(moments, " AND ")+`)`), nil
}

// labelSQL compares a choice's label with the labels wanted.
func labelSQL(c *column, op string, want []string, a *sqlArgs) (string, error) {
	switch op {
	case query.OperatorEmpty:
		return not(a.entry(c, "")), nil
	case query.OperatorNotEmpty:
		return a.entry(c, ""), nil
	case query.OperatorIs, query.OperatorIn, query.OperatorStatusGroupIs:
		return a.entry(c, `ci.t = ANY(`+a.add(want)+`::text[])`), nil
	case query.OperatorIsNot, query.OperatorNotIn:
		return not(a.entry(c, `ci.t = ANY(`+a.add(want)+`::text[])`)), nil
	case query.OperatorAllOf, query.OperatorIsExactly:
		// One label: all of them, or exactly them, is each one being it.
		set := map[string]bool{}
		for _, w := range want {
			set[w] = true
		}
		if len(set) != 1 {
			return "FALSE", nil
		}
		return a.entry(c, `ci.t = ANY(`+a.add(want)+`::text[])`), nil
	case query.OperatorContains, query.OperatorNotContains:
		if len(want) == 0 {
			break
		}
		return negatedIf(op == query.OperatorNotContains, a.entry(c, `strpos(ci.t, `+a.add(want[0])+`::text) > 0`)), nil
	}
	return "", cantQuery("%s can't be compared with %s", c.field.Name, op)
}

// setSQL compares a list entry's labels or ids with the values wanted.
func setSQL(c *column, op string, want []string, a *sqlArgs, labels bool) (string, error) {
	arr := func() string { return a.add(want) + `::text[]` }
	switch op {
	case query.OperatorEmpty:
		return not(a.entry(c, "")), nil
	case query.OperatorNotEmpty:
		return a.entry(c, ""), nil
	case query.OperatorIs, query.OperatorIn, query.OperatorStatusGroupIs:
		return a.entry(c, `ci.a && `+arr()), nil
	case query.OperatorIsNot, query.OperatorNotIn:
		return not(a.entry(c, `ci.a && `+arr())), nil
	case query.OperatorAllOf:
		if len(want) == 0 {
			return "TRUE", nil
		}
		return a.entry(c, `ci.a @> `+arr()), nil
	case query.OperatorIsExactly:
		if !labels {
			break
		}
		if len(want) == 0 {
			return not(a.entry(c, "")), nil
		}
		w := arr()
		return a.entry(c, `ci.a @> `+w+` AND ci.a <@ `+w), nil
	case query.OperatorContains, query.OperatorNotContains:
		if !labels || len(want) == 0 {
			break
		}
		return negatedIf(op == query.OperatorNotContains, a.entry(c, `EXISTS (SELECT 1 FROM unnest(ci.a) l WHERE strpos(l, `+a.add(want[0])+`::text) > 0)`)), nil
	}
	return "", cantQuery("can't compare with %s here", op)
}

// idsSQL compares a row column's person or a link's rows with the ids wanted.
func idsSQL(e, op string, want []string, a *sqlArgs) (string, error) {
	arr := func() string { return a.add(want) + `::text[]` }
	switch op {
	case query.OperatorEmpty:
		return `cardinality(` + e + `) = 0`, nil
	case query.OperatorNotEmpty:
		return `cardinality(` + e + `) > 0`, nil
	case query.OperatorIs, query.OperatorIn:
		return e + ` && ` + arr(), nil
	case query.OperatorIsNot, query.OperatorNotIn:
		return `NOT (` + e + ` && ` + arr() + `)`, nil
	case query.OperatorAllOf:
		return e + ` @> ` + arr(), nil
	}
	return "", cantQuery("people and links can't be compared with %s", op)
}

// sortKey is one sort as SQL: its expression and its direction. Nulls go
// last either way.
type sortKey struct {
	expr string
	desc bool
	// kind is how a cursor writes its value: text, num, time or bool.
	kind string
}

// joined is a sort's entry, joined to r once, as s<n>.
func (a *sqlArgs) joined(c *column) string {
	alias := "s" + strconv.Itoa(len(a.joins))
	a.joins = append(a.joins, `LEFT JOIN data_table_cell_index `+alias+` ON `+alias+`.row_id = r.id AND `+alias+`.field_id = `+a.add(c.key)+`::uuid`)
	return alias
}

// sortKeysOf is a spec's order (specOrder: its groups, then its sorts) as
// SQL, then the table's own order.
func sortKeysOf(sorts []sortBy, cols map[string]*column, who asker, a *sqlArgs) ([]sortKey, error) {
	var keys []sortKey
	for _, s := range sorts {
		c := cols[s.Field]
		if c == nil {
			return nil, errNoField(s.Field)
		}
		switch {
		case s.group && (c.family == famChoice || c.family == famDate):
			keys = append(keys, groupOrder(c, s, who, a)...)
		case c.col == colRowNo:
			keys = append(keys, sortKey{expr: `NULLIF(r.row_no, 0)::numeric`, desc: s.Desc, kind: "num"})
		case c.col == colCreatedAt:
			keys = append(keys, sortKey{expr: `r.created_at`, desc: s.Desc, kind: "time"})
		case c.col == colUpdatedAt:
			keys = append(keys, sortKey{expr: `r.updated_at`, desc: s.Desc, kind: "time"})
		case c.col != colNone:
			return nil, cantQuery("sorting by %s isn't built yet", c.field.Name)
		case c.family == famText:
			// By its natural key (sortKey.go): t is what a filter reads.
			keys = append(keys, sortKey{expr: a.joined(c) + `.s`, desc: s.Desc, kind: "text"})
		case c.family == famNumber:
			keys = append(keys, sortKey{expr: a.joined(c) + `.n`, desc: s.Desc, kind: "num"})
		case c.family == famDate:
			keys = append(keys, sortKey{expr: a.joined(c) + `.d`, desc: s.Desc, kind: "time"})
		case c.family == famCheckbox:
			keys = append(keys, sortKey{expr: `COALESCE(` + a.joined(c) + `.b, FALSE)`, desc: s.Desc, kind: "bool"})
		case c.family == famChoice:
			// By the options' order (its entry's n, optionPlace), then its
			// label's natural key.
			j := a.joined(c)
			keys = append(keys, sortKey{expr: j + `.n`, desc: s.Desc, kind: "num"}, sortKey{expr: j + `.s`, desc: s.Desc, kind: "text"})
		default:
			return nil, cantQuery("sorting by %s isn't built yet", c.field.Name)
		}
	}
	// The table's own order: its place, then its id. A row with no place yet
	// comes last.
	// By the uuid itself: its byte order is its text's, as the evaluator
	// compares ids, and the index on (table, place, id) serves it.
	keys = append(keys, sortKey{expr: `r.position_key`, kind: "text"}, sortKey{expr: `r.id`, kind: "uuid"})
	return keys, nil
}

// orderBy is the keys as an ORDER BY, text byte by byte ("C"), as Go
// compares it and as the position keys are written.
func orderBy(keys []sortKey) string {
	return orderOf(keys, false)
}

// orderByLast is orderBy backwards: the last row in the keys' order first.
func orderByLast(keys []sortKey) string {
	return orderOf(keys, true)
}

func orderOf(keys []sortKey, backwards bool) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		dir, nulls := "ASC", "NULLS LAST"
		if k.desc != backwards {
			dir = "DESC"
		}
		if backwards {
			nulls = "NULLS FIRST"
		}
		parts[i] = keyExpr(k) + ` ` + dir + ` ` + nulls
	}
	return "ORDER BY " + strings.Join(parts, ", ")
}

func keyExpr(k sortKey) string {
	if k.kind == "text" {
		return `(` + k.expr + `) COLLATE "C"`
	}
	return `(` + k.expr + `)`
}

// afterSQL is the condition that a row comes after the cursor's row in the
// keys' order: equal on the keys before one, and after it on that one, a
// NULL coming after every value (NULLS LAST, either way). The table's own
// order (its place, then its id, the last two keys) is one row comparison,
// which the index on (table, place, id) answers as a range; a row with no
// place yet sorts after every place, so unkeyed (the table has such rows)
// takes them in too. An id is never NULL.
func afterSQL(keys []sortKey, at []interface{}, a *sqlArgs, unkeyed bool) string {
	cast := map[string]string{"text": `::text COLLATE "C"`, "num": "::numeric", "time": "::timestamptz", "bool": "::boolean", "uuid": "::uuid"}
	var ors []string
	for i := range keys {
		if at[i] == nil {
			continue // after a NULL on this key come only NULLs, which tie
		}
		var ands []string
		for j := 0; j < i; j++ {
			if at[j] == nil {
				ands = append(ands, `(`+keys[j].expr+`) IS NULL`)
			} else {
				ands = append(ands, keyExpr(keys[j])+` = `+a.add(at[j])+cast[keys[j].kind])
			}
		}
		if i == len(keys)-2 { // the table's place, then its id: at[i+1] is never nil
			tail := `(r.position_key, r.id) > (` + a.add(at[i]) + `::text COLLATE "C", ` + a.add(at[i+1]) + `::uuid)`
			if unkeyed {
				tail = `(` + tail + ` OR r.position_key IS NULL)`
			}
			ors = append(ors, "("+strings.Join(append(ands, tail), " AND ")+")")
			break
		}
		cmp := ">"
		if keys[i].desc {
			cmp = "<"
		}
		after := keyExpr(keys[i]) + ` ` + cmp + ` ` + a.add(at[i]) + cast[keys[i].kind]
		if keys[i].kind != "uuid" {
			after = `(` + after + ` OR (` + keys[i].expr + `) IS NULL)`
		}
		ands = append(ands, after)
		ors = append(ors, "("+strings.Join(ands, " AND ")+")")
	}
	if len(ors) == 0 {
		return "FALSE"
	}
	return strings.Join(ors, " OR ")
}
