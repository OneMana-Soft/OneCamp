package business

// What each read works out for a page of rows, in order:
//  1. the links its relation fields make to tables' rows, by name;
//  2. the links other tables make to its rows;
//  3. rollups;
//  4. formulas, which can read all of these.
//
// The read has one budget for the work (formula.Budget): its own formulas,
// those of the tables it links to, and its rollups all draw on it. It also
// counts, fetches and loads only so many links and linked rows (readLinks,
// loadLinked), the first rows of the page first; cells past that say what
// they left out. Nothing here is stored: the values are spliced into each
// row's stored text (withValues), where every reader finds a cell.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/business/DataTable/formula"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

const (
	// maxLabel is the most characters of a linked row's name a link shows.
	maxLabel = 100
	// maxRollupText is the most characters a rollup listing values gives.
	maxRollupText = 10000
	// maxRolledUp is the most linked rows a rollup adds up for a row.
	maxRolledUp = 10000
	// maxCounted is the most links a cell counts.
	maxCounted = 100000
	// maxReadCounted is how many links one read counts, across its fields,
	// and maxCountQuery how many one query counts at most.
	maxReadCounted = 5000000
	maxCountQuery  = 500000
	// maxReadLinks is how many links one read fetches, across its fields.
	maxReadLinks = 200000
	// maxLinkedRows is how many linked rows one read works out (their names
	// and the fields rollups add up), across the tables it links to.
	maxLinkedRows = 50000
	// loadBatch is how many linked rows are read at a time.
	loadBatch = 2000
)

// What a read can leave out, said in the cells it leaves out: no answer,
// rather than a wrong one (formula.Short), so totals reading them say they
// fall short.
var (
	errLinksFailed    = formula.Short("The links couldn't be read")
	errTooManyLinked  = formula.Short("Too many linked rows to add up here")
	errLinksUncounted = formula.Short("Too many links on this page to count")
	errTooManyCounted = formula.Short("More than " + helpers.Thousands(maxCounted) + " linked rows")
	errTooManyToRead  = formula.Short("Too many links for a formula to read")
)

// computed is what a table's computed fields need, found once for a read.
type computed struct {
	own     uuid.UUID
	fields  []*model.Field
	types   map[string]string
	budget  *formula.Budget
	program *formula.Program
	links   []tableLink
	rollups []*rollupField
	targets map[uuid.UUID]*target
	// order is the targets in the order the fields name them.
	order    []uuid.UUID
	problems map[string]string // why a field can't be worked out, by id
	// rolledUp is the table links whose linked rows rollups add up, by field
	// id: a read fetches all their links, not just those shown.
	rolledUp map[string]bool
	// linked is each table link's links for the page, by field id; none for
	// one whose links aren't shown to the reader.
	linked map[string]*pageLinks
	// counted and fetched are the links the read has counted and fetched.
	counted, fetched int
}

// pageLinks is a table link's links for a page.
type pageLinks struct {
	// count is how many links each row has: at most maxCounted+1, which is
	// more than maxCounted.
	count map[uuid.UUID]int
	// uncounted is the rows past the links one read counts; their count is
	// as far as it got.
	uncounted map[uuid.UUID]bool
	// of is the links read for each row, in order: those it shows or, when a
	// rollup adds them up, all of them, as far as one read fetches.
	of map[uuid.UUID][]uuid.UUID
	// failed is set when they couldn't be read.
	failed bool
}

// rollupField is a rollup, with the relation it reads and the field it adds
// up, once checked.
type rollupField struct {
	field *model.Field
	cfg   rollupConfig
	link  tableLink
	of    *model.Field
}

// target is a table a read's relation fields link to.
type target struct {
	table   *model.DataTable // nil once deleted
	visible bool
	fields  []*model.Field
	// label is the field whose value names a row.
	label *model.Field
	// reads are the fields rollups add up here, in order, and readAt where
	// each is among them, by id.
	reads  []*model.Field
	readAt map[string]int
	rows   map[uuid.UUID]*linkedRow
	// skipped is the linked rows left out, past what one read works out.
	skipped map[uuid.UUID]bool
}

func newTarget() *target {
	return &target{readAt: map[string]int{}, rows: map[uuid.UUID]*linkedRow{}, skipped: map[uuid.UUID]bool{}}
}

// linkedRow is a linked row as a read works it out: its name, and the value
// of each field rollups add up (target.reads).
type linkedRow struct {
	label  string
	values []formula.Value
}

// read is the value of a field rollups add up in a linked row of t.
func (t *target) read(lr *linkedRow, field string) formula.Value {
	return lr.values[t.readAt[field]]
}

// planComputed finds what a table's computed fields need: the tables its
// relation fields link to, and whether each rollup can be worked out.
func planComputed(ctx context.Context, fields []*model.Field) *computed {
	return planComputedFor(ctx, fields, nil)
}

// planComputedFor is planComputed for only the computed fields keep names,
// by id, or all of them when it's nil.
func planComputedFor(ctx context.Context, fields []*model.Field, keep map[string]bool) *computed {
	c := &computed{
		fields:   fields,
		types:    make(map[string]string, len(fields)),
		budget:   formula.NewBudget(),
		targets:  map[uuid.UUID]*target{},
		problems: map[string]string{},
		rolledUp: map[string]bool{},
		linked:   map[string]*pageLinks{},
	}
	byID := make(map[string]*model.Field, len(fields))
	for _, f := range fields {
		c.own = f.TableId
		c.types[f.Id.String()] = f.Type
		byID[f.Id.String()] = f
	}
	kept := func(f *model.Field) bool { return keep == nil || keep[f.Id.String()] }
	for _, f := range fields {
		if l, ok := linkOf(f); ok && kept(f) {
			c.links = append(c.links, l)
			c.target(ctx, l.table)
		}
	}
	for _, f := range fields {
		if cfg, ok := rollupOf(f); ok && kept(f) {
			r := &rollupField{field: f, cfg: cfg}
			if why := c.checkRollup(ctx, r, byID); why != "" {
				c.problems[f.Id.String()] = why
			} else if cfg.Aggregate != "count" {
				c.rolledUp[r.link.field.Id.String()] = true
			}
			c.rollups = append(c.rollups, r)
		}
	}
	in := formulaInputs(fields)
	if keep != nil {
		in = onlyFormulas(fields, keep)
	}
	c.program = formula.Compile(in).Within(c.budget)
	return c
}

// computedNeeds is the computed fields that reading the fields ids takes, by
// id: those of them that are computed, the formulas, links and rollups those
// read, and the relations the rollups add up.
func computedNeeds(fields []*model.Field, ids []string) map[string]bool {
	byID := make(map[string]*model.Field, len(fields))
	for _, f := range fields {
		byID[f.Id.String()] = f
	}
	keep := map[string]bool{}
	var need func(id string)
	need = func(id string) {
		f := byID[id]
		if f == nil || keep[id] {
			return
		}
		if cfg, ok := rollupOf(f); ok {
			keep[id] = true
			need(cfg.Relation)
		} else if f.Type == model.FieldFormula {
			keep[id] = true
			for _, ref := range formula.Refs(storedFormula(f)) {
				need(strings.TrimPrefix(ref, "#"))
			}
		} else if _, isLink := linkOf(f); isLink {
			keep[id] = true
		}
	}
	for _, id := range ids {
		need(id)
	}
	return keep
}

// labelField is the field whose value names a table's rows: the first with
// a value of its own, or a formula that doesn't read links or rollups.
func labelField(fields []*model.Field) *model.Field {
	for _, f := range fields {
		if _, isLink := linkOf(f); isLink || f.Type == model.FieldRollup {
			continue
		}
		if f.Type == model.FieldFormula && readsComputed(f, fields) {
			continue
		}
		return f
	}
	return nil
}

// target is the table a relation field links to, loaded once a read.
func (c *computed) target(ctx context.Context, id uuid.UUID) *target {
	if t, ok := c.targets[id]; ok {
		return t
	}
	t := newTarget()
	c.targets[id] = t
	c.order = append(c.order, id)
	tb, err := model.GetTableByID(ctx, id)
	if err != nil || tb == nil {
		return t
	}
	t.table, t.visible = tb, mayOpen(ctx, tb, c.own)
	if !t.visible {
		return t
	}
	if id == c.own {
		t.fields = c.fields
	} else if t.fields, err = model.ListFields(ctx, id); err != nil {
		t.visible = false
		return t
	}
	t.label = labelField(t.fields)
	return t
}

// checkRollup finds what a rollup reads, or why it can't be worked out.
func (c *computed) checkRollup(ctx context.Context, r *rollupField, byID map[string]*model.Field) string {
	if _, ok := rollupGives[r.cfg.Aggregate]; !ok {
		return "Choose how to add it up"
	}
	l, ok := linkOf(byID[r.cfg.Relation])
	if !ok {
		return "The relation this adds up has been deleted"
	}
	r.link = l
	t := c.target(ctx, l.table)
	switch {
	case t.table == nil:
		return "The table this adds up has been deleted"
	case !t.visible:
		return "You can't open the table this adds up"
	case r.cfg.Aggregate == "count":
		return ""
	}
	for _, f := range t.fields {
		if f.Id.String() == r.cfg.Field {
			r.of = f
		}
	}
	if r.of == nil {
		return "The field this adds up has been deleted"
	}
	if why := rollupProblem(r.cfg.Aggregate, r.of, t.fields); why != "" {
		return why
	}
	if _, ok := t.readAt[r.of.Id.String()]; !ok {
		t.readAt[r.of.Id.String()] = len(t.reads)
		t.reads = append(t.reads, r.of)
	}
	return ""
}

// rollupProblem is why a rollup can't add up field of in a table with fields
// that way, or "".
func rollupProblem(how string, of *model.Field, fields []*model.Field) string {
	if of.Type == model.FieldRollup {
		return "A rollup can't add up another rollup"
	}
	if _, isLink := linkOf(of); isLink {
		return "A rollup can't add up links to another table"
	}
	if of.Type == model.FieldFormula && readsComputed(of, fields) {
		return "A rollup can't add up a formula that reads links or rollups"
	}
	kind := kindOfField(of)
	if of.Type == model.FieldFormula {
		kind = formula.Compile(formulaInputs(fields)).Kind(of.Id.String())
	}
	if !rollupFits(how, kind) {
		return "That way of adding up doesn't fit this field"
	}
	return ""
}

// readsComputed is whether a formula reads, itself or through other
// formulas, a field linking to a table or a rollup: values a rollup's read of
// another table doesn't work out.
func readsComputed(f *model.Field, fields []*model.Field) bool {
	byID := make(map[string]*model.Field, len(fields))
	for _, g := range fields {
		byID[g.Id.String()] = g
	}
	seen := map[string]bool{}
	var reads func(g *model.Field) bool
	reads = func(g *model.Field) bool {
		if g == nil || seen[g.Id.String()] {
			return false
		}
		seen[g.Id.String()] = true
		if _, isLink := linkOf(g); isLink || g.Type == model.FieldRollup {
			return true
		}
		if g.Type != model.FieldFormula {
			return false
		}
		for _, ref := range formula.Refs(storedFormula(g)) {
			if reads(byID[strings.TrimPrefix(ref, "#")]) {
				return true
			}
		}
		return false
	}
	return reads(f)
}

// apply works out a page of rows' computed cells and splices them in.
func (c *computed) apply(ctx context.Context, rows []*model.Row) {
	if (c.program.Empty() && len(c.links) == 0 && len(c.rollups) == 0) || len(rows) == 0 {
		return
	}
	page := make([]*pageRow, 0, len(rows))
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if r != nil {
			page = append(page, newPageRow(r))
			ids = append(ids, r.Id)
		}
	}
	now, loc := time.Now(), zoneFrom(ctx)
	c.readLinks(ctx, ids)
	c.loadLinked(ctx, ids, now, loc)
	for _, pr := range page {
		row := pr.row.Id
		out := map[string]interface{}{}
		values := map[string]formula.Value{}
		for _, l := range c.links {
			fid := l.field.Id.String()
			refs, v := c.linkCell(l, row)
			if len(refs) > 0 {
				out[fid] = refs
			} else {
				out[fid] = nil
			}
			values[fid] = v
		}
		for _, r := range c.rollups {
			v := c.rollUp(r, row)
			out[r.field.Id.String()] = v.JSON()
			values[r.field.Id.String()] = v
		}
		if !c.program.Empty() {
			worked := c.program.Run(func(id string) (formula.Value, int) {
				if v, ok := values[id]; ok {
					return v, 0
				}
				return pr.cell(id, c.types[id])
			}, now, loc)
			for id, v := range worked {
				out[id] = v.JSON()
			}
		}
		pr.row.Values = withValues(pr.row.Values, pr.cells, out)
	}
}

// readLinks reads each table link's links for a page of rows, ids: how many
// each row has, then the first ones, which it shows, or, for one rollups add
// up, all of them. The first rows of the page come first: a row past the
// links one read counts or fetches has its cells say so.
func (c *computed) readLinks(ctx context.Context, ids []uuid.UUID) {
	var read []tableLink
	for _, l := range c.links {
		t := c.targets[l.table]
		field, ok := l.linkField()
		// Links from a table the reader can't open aren't shown at all.
		if t.table == nil || !ok || (l.inverseOf != "" && !t.visible) {
			continue
		}
		pl := &pageLinks{count: make(map[uuid.UUID]int, len(ids)), uncounted: map[uuid.UUID]bool{}}
		if err := c.countLinks(ctx, field, l.inverseOf != "", ids, maxLinks+1, pl); err != nil {
			c.linked[l.field.Id.String()] = &pageLinks{failed: true}
			continue
		}
		c.linked[l.field.Id.String()] = pl
		read = append(read, l)
	}
	// Rows with more links than a cell can make are counted further once
	// every field has its first counts. If that fails, as when there are a
	// great many to count, the rest are left uncounted.
	for _, l := range read {
		pl := c.linked[l.field.Id.String()]
		var more []uuid.UUID
		for _, id := range ids {
			if pl.count[id] == maxLinks+1 && !pl.uncounted[id] {
				more = append(more, id)
			}
		}
		field, _ := l.linkField()
		if c.countLinks(ctx, field, l.inverseOf != "", more, maxCounted+1, pl) != nil {
			c.counted = maxReadCounted
		}
	}
	// How many of each row's links to fetch: first those each cell shows, a
	// row at a time across the fields, then, for the fields rollups add up,
	// the rest of each row's, as far as what one read fetches goes.
	limits := make(map[string][]int, len(read))
	for _, l := range read {
		limits[l.field.Id.String()] = make([]int, len(ids))
	}
	for pass := 0; pass < 2; pass++ {
		for i, id := range ids {
			for _, l := range read {
				fid := l.field.Id.String()
				pl := c.linked[fid]
				n, has := pl.count[id], limits[fid][i]
				want := min(n, maxShownLinks)
				if pass == 1 {
					if !c.rolledUp[fid] || pl.uncounted[id] || n > maxRolledUp || has != want {
						continue
					}
					want = n
				}
				if want > has && c.fetched+want-has <= maxReadLinks {
					c.fetched += want - has
					limits[fid][i] = want
				}
			}
		}
	}
	for _, l := range read {
		fid := l.field.Id.String()
		var rows []uuid.UUID
		var ks []int
		for i, k := range limits[fid] {
			if k > 0 {
				rows, ks = append(rows, ids[i]), append(ks, k)
			}
		}
		field, _ := l.linkField()
		of, err := model.LinksOf(ctx, field, rows, ks, l.inverseOf != "")
		if err != nil {
			c.linked[fid] = &pageLinks{failed: true}
			continue
		}
		c.linked[fid].of = of
	}
}

// countLinks counts a field's links from rows ids (to them, when to is set)
// into pl, at most upTo a row: as many rows a query as it can count in full
// within maxCountQuery and what's left of what one read counts. Rows past
// that, or in a query that fails, are left uncounted (with the count they
// had).
func (c *computed) countLinks(ctx context.Context, field uuid.UUID, to bool, ids []uuid.UUID, upTo int, pl *pageLinks) error {
	for len(ids) > 0 {
		n := min(maxCountQuery, maxReadCounted-c.counted) / upTo
		chunk := ids[:min(max(n, 0), len(ids))]
		var got map[uuid.UUID]int
		var err error
		if len(chunk) > 0 {
			got, err = model.CountLinks(ctx, field, chunk, upTo, to)
		}
		if len(chunk) == 0 || err != nil {
			for _, id := range ids {
				pl.uncounted[id] = true
			}
			return err
		}
		for _, id := range chunk {
			pl.count[id] = got[id]
			c.counted += got[id]
		}
		ids = ids[len(chunk):]
	}
	return nil
}

// loadLinked reads the linked rows a page needs, up to maxLinkedRows: first
// those its cells show, a row of the page at a time across the fields, then
// those rollups add up. The rest are left out (target.skipped).
func (c *computed) loadLinked(ctx context.Context, ids []uuid.UUID, now time.Time, loc *time.Location) {
	wanted := map[uuid.UUID][]uuid.UUID{}
	seen := map[uuid.UUID]bool{}
	n := 0
	for pass := 0; pass < 2; pass++ {
		for _, id := range ids {
			for _, l := range c.links {
				t, pl := c.targets[l.table], c.linked[l.field.Id.String()]
				if pl == nil || !t.visible {
					continue
				}
				of := pl.of[id]
				part := of[:min(len(of), maxShownLinks)]
				if pass == 1 {
					part = of[len(part):]
				}
				for _, other := range part {
					if seen[other] {
						continue
					}
					seen[other] = true
					if n == maxLinkedRows {
						t.skipped[other] = true
						continue
					}
					n++
					wanted[l.table] = append(wanted[l.table], other)
				}
			}
		}
	}
	for _, id := range c.order {
		c.load(ctx, c.targets[id], wanted[id], now, loc)
	}
}

// needs is what reading a target's rows needs: the stored cells, by field
// id, and the formulas to work out, for the rows' names and the fields
// rollups add up there.
func (t *target) needs() ([]string, map[string]bool) {
	byID := make(map[string]*model.Field, len(t.fields))
	for _, f := range t.fields {
		byID[f.Id.String()] = f
	}
	keys, formulas, seen := []string{}, map[string]bool{}, map[string]bool{}
	var need func(f *model.Field)
	need = func(f *model.Field) {
		if f == nil || seen[f.Id.String()] {
			return
		}
		seen[f.Id.String()] = true
		if f.Type != model.FieldFormula {
			keys = append(keys, f.Id.String())
			return
		}
		formulas[f.Id.String()] = true
		for _, ref := range formula.Refs(storedFormula(f)) {
			need(byID[strings.TrimPrefix(ref, "#")])
		}
	}
	need(t.label)
	for _, f := range t.reads {
		need(f)
	}
	return keys, formulas
}

// onlyFormulas is a table's fields as formulas see them, with only the
// formulas keep worked out: the others read as plain, empty fields.
func onlyFormulas(fields []*model.Field, keep map[string]bool) []formula.Field {
	in := formulaInputs(fields)
	for i := range in {
		if in[i].IsFormula && !keep[in[i].ID] {
			in[i].IsFormula, in[i].Formula = false, ""
		}
	}
	return in
}

// load reads the linked rows of a table a read needs: each one's name, and
// the fields rollups add up there, with only the cells and formulas those
// need, worked out on the read's budget. Rows it couldn't read are left out
// (target.skipped).
func (c *computed) load(ctx context.Context, t *target, ids []uuid.UUID, now time.Time, loc *time.Location) {
	if t.table == nil || !t.visible || len(ids) == 0 {
		return
	}
	keys, formulas := t.needs()
	types := make(map[string]string, len(t.fields))
	for _, f := range t.fields {
		types[f.Id.String()] = f.Type
	}
	var program *formula.Program
	if len(formulas) > 0 {
		program = formula.Compile(onlyFormulas(t.fields, formulas)).Within(c.budget)
	}
	for len(ids) > 0 {
		batch := ids[:min(loadBatch, len(ids))]
		ids = ids[len(batch):]
		rows, err := model.CellsOf(ctx, t.table.Id, batch, keys)
		if err != nil {
			for _, left := range [][]uuid.UUID{batch, ids} {
				for _, id := range left {
					t.skipped[id] = true
				}
			}
			return
		}
		for _, r := range rows {
			pr := newPageRow(r)
			var worked map[string]formula.Value
			if program != nil {
				worked = program.Run(func(id string) (formula.Value, int) { return pr.cell(id, types[id]) }, now, loc)
			}
			value := func(f *model.Field) formula.Value {
				if f.Type == model.FieldFormula {
					return worked[f.Id.String()]
				}
				v, _ := pr.cell(f.Id.String(), f.Type)
				return v
			}
			lr := &linkedRow{values: make([]formula.Value, len(t.reads))}
			if t.label != nil {
				lr.label = clipLabel(value(t.label).Text())
			}
			for i, f := range t.reads {
				lr.values[i] = value(f)
			}
			t.rows[r.Id] = lr
		}
	}
}

// clipLabel is a linked row's name as a link shows it: at most maxLabel
// characters, and "Untitled" when it has none.
func clipLabel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Untitled"
	}
	if utf8.RuneCountInString(s) > maxLabel {
		return string([]rune(s)[:maxLabel]) + "…"
	}
	return s
}

// linkRef is a link as a relation cell shows it: a linked row ("row"), or
// how many more there are ("more").
type linkRef struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Type    string `json:"type"`
	TableID string `json:"table_id"`
}

// linkCell is a relation cell as readers get it, and the value formulas
// read for it. The cell is a ref for each linked row it shows, up to
// maxShownLinks, then one saying how many more there are, or that they
// weren't all read; links to a table the reader can't open show privateRow.
// Formulas read the linked rows' names, when the cell shows them all.
func (c *computed) linkCell(l tableLink, row uuid.UUID) ([]linkRef, formula.Value) {
	t, pl := c.targets[l.table], c.linked[l.field.Id.String()]
	if t == nil || t.table == nil || pl == nil {
		return nil, formula.Text("")
	}
	table := t.table.Id.String()
	more := func(label string) linkRef {
		return linkRef{Label: label, Type: "more", TableID: table}
	}
	if pl.failed {
		return []linkRef{more("Links couldn't be read")}, errLinksFailed
	}
	of := pl.of[row]
	shown := of[:min(len(of), maxShownLinks)]
	n := pl.count[row]
	whole := !pl.uncounted[row] && n == len(shown)
	refs := make([]linkRef, 0, len(shown)+1)
	names := make([]string, 0, len(shown))
	for _, id := range shown {
		label := privateRow
		if t.visible {
			r, ok := t.rows[id]
			switch {
			case ok:
				label = r.label
			case t.skipped[id]:
				label, whole = "…", false // past what one read works out
			default:
				continue // deleted since
			}
		}
		refs = append(refs, linkRef{ID: id.String(), Label: label, Type: "row", TableID: table})
		names = append(names, label)
	}
	switch rest := n - len(shown); {
	case pl.uncounted[row] && n == 0:
		refs = append(refs, more("Links not counted here"))
	case pl.uncounted[row] || n > maxCounted:
		refs = append(refs, more(helpers.Thousands(int64(rest))+"+ more"))
	case rest > 0 && len(shown) == 0:
		refs = append(refs, more(helpers.Thousands(int64(n))+plural(n, " link", " links")))
	case rest > 0:
		refs = append(refs, more(helpers.Thousands(int64(rest))+" more"))
	}
	if !whole {
		return refs, errTooManyToRead
	}
	return refs, formula.Text(strings.Join(names, ", "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// rollUp works out a rollup for a row, from the rows it links to.
func (c *computed) rollUp(r *rollupField, row uuid.UUID) formula.Value {
	if why := c.problems[r.field.Id.String()]; why != "" {
		return formula.Error(why)
	}
	pl := c.linked[r.link.field.Id.String()]
	if pl == nil || pl.failed {
		return errLinksFailed
	}
	n := pl.count[row]
	switch {
	case pl.uncounted[row]:
		return errLinksUncounted
	case r.cfg.Aggregate != "count":
	case n > maxCounted:
		return errTooManyCounted
	default:
		return formula.Number(float64(n))
	}
	others := pl.of[row]
	if n > len(others) {
		return errTooManyLinked // more than a rollup adds up, or past what the read fetches
	}
	t := c.targets[r.link.table]
	acc := newRollupAcc(r.cfg.Aggregate)
	for _, id := range others {
		lr, ok := t.rows[id]
		if !ok {
			if t.skipped[id] {
				return errTooManyLinked
			}
			continue // deleted since
		}
		v := t.read(lr, r.of.Id.String())
		if v.IsUnfinished() || !c.budget.Spend(1+len(v.Str)/32) {
			return formula.RanOut()
		}
		if acc.add(v) {
			break
		}
	}
	return acc.result()
}

// rollupAcc adds up a rollup's values one at a time, in place.
type rollupAcc struct {
	how           string
	given         int // values that aren't blank or errors
	checked       int
	nums          int
	total, lo, hi float64
	pick          formula.Value
	picked        bool
	seen          map[string]bool
	text          strings.Builder
	chars         int
	tooLong       bool
}

func newRollupAcc(how string) *rollupAcc {
	return &rollupAcc{how: how, seen: map[string]bool{}}
}

// add takes one linked row's value, and says when no more can change the
// answer.
func (a *rollupAcc) add(v formula.Value) bool {
	if v.Kind == formula.KindBlank || v.Kind == formula.KindError {
		return false
	}
	a.given++
	switch a.how {
	case "checked":
		if v.Kind == formula.KindBool && v.Bool {
			a.checked++
		}
	case "sum", "average", "min", "max":
		if v.Kind == formula.KindNumber {
			if a.nums == 0 {
				a.lo, a.hi = v.Num, v.Num
			}
			a.total, a.lo, a.hi = a.total+v.Num, min(a.lo, v.Num), max(a.hi, v.Num)
			a.nums++
		}
	case "earliest", "latest":
		if v.Kind == formula.KindDate && (!a.picked || (a.how == "earliest" && v.Time.Before(a.pick.Time)) || (a.how == "latest" && v.Time.After(a.pick.Time))) {
			a.pick, a.picked = v, true
		}
	case "unique_count":
		a.seen[v.Text()] = true
	case "list", "unique":
		s := v.Text()
		if a.how == "unique" {
			if a.seen[s] {
				return false
			}
			a.seen[s] = true
		}
		if a.text.Len() > 0 {
			a.text.WriteString(", ")
			a.chars += 2
		}
		a.text.WriteString(s)
		if a.chars += utf8.RuneCountInString(s); a.chars > maxRollupText {
			a.tooLong = true
			return true
		}
	}
	return false
}

// result is what the values added up to.
func (a *rollupAcc) result() formula.Value {
	switch a.how {
	case "count_values":
		return formula.Number(float64(a.given))
	case "unique_count":
		return formula.Number(float64(len(a.seen)))
	case "checked":
		return formula.Number(float64(a.checked))
	case "sum":
		return formula.Number(a.total)
	case "average", "min", "max":
		if a.nums == 0 {
			return formula.Blank()
		}
		switch a.how {
		case "average":
			return formula.Number(a.total / float64(a.nums))
		case "min":
			return formula.Number(a.lo)
		}
		return formula.Number(a.hi)
	case "earliest", "latest":
		if !a.picked {
			return formula.Blank()
		}
		return a.pick
	case "list", "unique":
		if a.tooLong {
			return formula.Error(fmt.Sprintf("The list would be longer than %d characters", maxRollupText))
		}
		return formula.Text(a.text.String())
	}
	return formula.Error("Choose how to add it up")
}

// pageRow is a row of a read, its cells found but not decoded.
type pageRow struct {
	row   *model.Row
	cells []member
	at    map[string]int
}

func newPageRow(r *model.Row) *pageRow {
	cells, ok := rowMembers(r.Values)
	if !ok {
		cells, r.Values = canonicalMembers(r.Values)
	}
	at := make(map[string]int, len(cells))
	for i, m := range cells {
		at[m.key] = i // a key given twice reads as its last, as in encoding/json
	}
	return &pageRow{row: r, cells: cells, at: at}
}

// cell is a stored cell as a formula reads it, and what reading it cost.
func (p *pageRow) cell(id, fieldType string) (formula.Value, int) {
	i, ok := p.at[id]
	if !ok {
		return cellValue(fieldType, nil), 0
	}
	raw := p.row.Values[p.cells[i].value:p.cells[i].end]
	var v interface{}
	_ = json.Unmarshal([]byte(raw), &v)
	return cellValue(fieldType, v), formula.ReadCost(raw)
}

// presentComputedFields gives each computed field's config as readers want
// it:
//   - a formula with its fields by name, what it gives ("result": number,
//     text, date or checkbox) and, when it can't be worked out, why
//     ("error");
//   - a rollup with what it gives, and why it can't be worked out;
//   - a relation linking to a table with that table's name ("table_name"),
//     or why it can't link to it.
func presentComputedFields(fields []*model.Field, c *computed) {
	inputs := formulaInputs(fields)
	for _, f := range fields {
		cfg := map[string]interface{}{}
		switch f.Type {
		case model.FieldFormula:
			_ = json.Unmarshal([]byte(f.Config), &cfg)
			cfg["formula"] = formula.Display(storedFormula(f), inputs)
			cfg["result"] = c.program.Kind(f.Id.String()).Name()
			delete(cfg, "error")
			if err := c.program.Err(f.Id.String()); err != nil {
				cfg["error"] = formula.Message(err)
			}
		case model.FieldRollup:
			_ = json.Unmarshal([]byte(f.Config), &cfg)
			cfg["result"] = kindOfField(f).Name()
			delete(cfg, "error")
			if why := c.problems[f.Id.String()]; why != "" {
				cfg["error"] = why
			}
		case model.FieldRelation:
			l, ok := linkOf(f)
			if !ok {
				continue
			}
			_ = json.Unmarshal([]byte(f.Config), &cfg)
			delete(cfg, "error")
			delete(cfg, "table_name")
			if t := c.targets[l.table]; t == nil || t.table == nil {
				cfg["error"] = "The table this links to has been deleted"
			} else if t.visible {
				cfg["table_name"] = t.table.Name
			}
		default:
			continue
		}
		if b, err := json.Marshal(cfg); err == nil {
			f.Config = string(b)
		}
	}
}
