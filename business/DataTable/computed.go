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
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
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
	// maxRollupDepth is how many rollups deep a rollup's value goes, itself
	// counted: a rollup can add up a rollup that adds up another.
	maxRollupDepth = 3
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

// Why a rollup can't add up another rollup: the rollups it would go through
// go too deep, or come back round (rollupChainProblem), or that rollup can't
// be worked out (planDeeper).
var (
	rollupsTooDeep  = fmt.Sprintf("A rollup can't add up rollups more than %d levels deep", maxRollupDepth)
	rollupsGoRound  = "A rollup can't add up rollups that add each other up"
	rollupNotWorked = "The rollup this adds up can't be worked out"
)

// computed is what a table's computed fields need, found once for a read.
type computed struct {
	own     uuid.UUID
	fields  []*model.Field
	types   map[string]string
	live    map[string]bool // the table's fields, by id: a read keeps their cells alone
	budget  *formula.Budget
	program *formula.Program
	links   []tableLink
	rollups []*rollupField
	// linkedReads is each {Link}.{Field} the formulas read, by its
	// formula.LinkedRef: a list rollup of that field over that link, worked
	// out like one but never shown as a cell.
	linkedReads map[string]*rollupField
	// items is the relation fields linking to OneCamp items (itemLinks.go).
	items []itemField
	// stamps is the fields worked out from the row itself (its number, when
	// it was made and last changed, and by whom: business/FieldTypes'
	// FromRow), and stampOf each one's.
	stamps  []*model.Field
	stampOf []func(*model.Field, *model.Row) any
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
	// reach is what the read has taken in, shared with the reads one level
	// deeper that work out the rollups its rollups add up (planDeeper).
	*reach
	// depth is how far down those reads this one is: 0 for a table's own.
	depth int
	// now and loc are the read's moment and the reader's zone.
	now time.Time
	loc *time.Location
}

// reach is what one read takes in, however deep its rollups go: the links it
// has counted and fetched, the linked rows it has worked out, and each table
// it opens, opened once.
type reach struct {
	counted, fetched, loaded int
	tables                   map[uuid.UUID]*opened
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
	// cond is where its conditions are among its target's (target.conds),
	// counted from 1, so a rollup without conditions has 0; program is its
	// formula, when it adds up by one.
	cond    int
	program *formula.Program
	// sorted is where the order of its linked rows (rollupConfig's Sort and
	// Desc) is among its target's sorts, counted from 1: 0 for link order.
	sorted int
	// chain is the tables checkRollup went down past the one it links to,
	// adding up a rollup: each one rollupChainProblem opened on the way.
	chain []uuid.UUID
}

// opened is a table as a read opens it: whether its reader may, its fields,
// and which of them names a row.
type opened struct {
	table   *model.DataTable // nil once deleted
	visible bool
	fields  []*model.Field
	// label is the field whose value names a row.
	label *model.Field
}

// target is a table a read's relation fields link to.
type target struct {
	opened
	// reads are the fields rollups add up here, in order, and readAt where
	// each is among them, by id.
	reads  []*model.Field
	readAt map[string]int
	rows   map[uuid.UUID]*linkedRow
	// skipped is the linked rows left out, past what one read works out.
	skipped map[uuid.UUID]bool
	// conds is the rollups' conditions here, each checked once a linked
	// row (linkedRow.passes).
	conds []*rollupCond
	// sorts is the orders rollups put the linked rows in here, each read
	// once a linked row (linkedRow.keys).
	sorts []*rollupSort
	// deeper is the rollups here that rollups add up, by id, and sub the
	// read one level deeper that works them out for the linked rows.
	deeper []string
	sub    *computed
}

func newTarget() *target {
	return &target{readAt: map[string]int{}, rows: map[uuid.UUID]*linkedRow{}, skipped: map[uuid.UUID]bool{}}
}

// linkedRow is a linked row as a read works it out: its name, and the value
// of each field rollups add up (target.reads).
type linkedRow struct {
	label  string
	values []formula.Value
	// passes is whether the row meets each of its target's conds, and keys
	// its place in each of its target's sorts.
	passes []bool
	keys   []rowQuery.SortKey
}

// read is the value of a field rollups add up in a linked row of t.
func (t *target) read(lr *linkedRow, field string) formula.Value {
	return lr.values[t.readAt[field]]
}

// reading has t's linked rows read f, once however many rollups read it.
func (t *target) reading(f *model.Field) {
	if _, ok := t.readAt[f.Id.String()]; !ok {
		t.readAt[f.Id.String()] = len(t.reads)
		t.reads = append(t.reads, f)
	}
}

// planComputed finds what a table's computed fields need: the tables its
// relation fields link to, and whether each rollup can be worked out.
func planComputed(ctx context.Context, fields []*model.Field) *computed {
	return planComputedFor(ctx, fields, nil)
}

// planComputedFor is planComputed for only the computed fields keep names,
// by id, or all of them when it's nil. A field in the trash is left out,
// whoever passes it: its links wait there for its restore (D14) and nothing
// reads them, so a rollup over it says its relation has been deleted.
func planComputedFor(ctx context.Context, fields []*model.Field, keep map[string]bool) *computed {
	return planUnder(ctx, fields, keep, nil)
}

// planUnder is planComputedFor one level under the read above, on its
// budget and reach, or for a read of its own when above is nil.
func planUnder(ctx context.Context, fields []*model.Field, keep map[string]bool, above *computed) *computed {
	fields = liveOnly(fields)
	c := &computed{
		fields:   fields,
		types:    make(map[string]string, len(fields)),
		live:     make(map[string]bool, len(fields)),
		budget:   formula.NewBudget(),
		targets:  map[uuid.UUID]*target{},
		problems: map[string]string{},
		rolledUp: map[string]bool{},
		linked:   map[string]*pageLinks{},
		reach:    &reach{tables: map[uuid.UUID]*opened{}},
	}
	if above != nil {
		c.budget, c.reach, c.depth = above.budget, above.reach, above.depth+1
	}
	byID := make(map[string]*model.Field, len(fields))
	for _, f := range fields {
		c.own = f.TableId
		c.types[f.Id.String()] = f.Type
		c.live[f.Id.String()] = true
		byID[f.Id.String()] = f
	}
	kept := func(f *model.Field) bool { return keep == nil || keep[f.Id.String()] }
	for _, f := range fields {
		if l, ok := linkOf(f); ok && kept(f) {
			c.links = append(c.links, l)
			c.target(ctx, l.table)
		}
		if target, ok := itemsOf(f); ok && kept(f) {
			c.items = append(c.items, itemField{field: f, target: target})
		}
		if from := rowStamp(f); from != nil && kept(f) {
			c.stamps = append(c.stamps, f)
			c.stampOf = append(c.stampOf, from)
		}
	}
	for _, f := range fields {
		if cfg, ok := rollupOf(f); ok && kept(f) {
			r := &rollupField{field: f, cfg: cfg}
			if why := c.checkRollup(ctx, r, byID); why != "" {
				c.problems[f.Id.String()] = why
			} else if cfg.Aggregate != "count" || r.cond > 0 {
				// A count with conditions reads the rows it counts.
				c.rolledUp[r.link.field.Id.String()] = true
			}
			c.rollups = append(c.rollups, r)
		}
	}
	in := formulaInputs(fields)
	if keep != nil {
		in = onlyFormulas(fields, keep)
	}
	// Only formulas read linked rows' fields ({Link}.{Field}): a read one
	// level deeper, which works out rollups alone, opens no more tables.
	if slices.ContainsFunc(in, func(f formula.Field) bool { return f.IsFormula }) {
		in = withLinked(in, fields, func(table uuid.UUID) []*model.Field {
			if t := c.target(ctx, table); t.visible {
				return t.fields
			}
			return nil
		})
	}
	c.program = formula.Compile(in).Within(c.budget)
	c.planLinkedReads(ctx, byID)
	c.planDeeper(ctx)
	return c
}

// planDeeper plans, for each table this read links to whose rollups its
// rollups add up (target.deeper), the read one level deeper that works
// those out for the linked rows, on this read's budget and reach. Their
// chains were checked (rollupChainProblem), so each level down holds
// shorter ones. A rollup adding up one that can't be worked out says so.
func (c *computed) planDeeper(ctx context.Context) {
	for _, id := range c.order {
		if t := c.targets[id]; len(t.deeper) > 0 {
			t.sub = planUnder(ctx, t.fields, computedNeeds(t.fields, t.deeper), c)
		}
	}
	notWorked := func(r *rollupField) bool {
		if c.problems[r.field.Id.String()] != "" || r.of == nil || r.of.Type != model.FieldRollup {
			return false
		}
		return c.targets[r.link.table].sub.problems[r.of.Id.String()] != ""
	}
	for _, r := range c.rollups {
		if notWorked(r) {
			c.problems[r.field.Id.String()] = rollupNotWorked
		}
	}
	for _, r := range c.linkedReads {
		if notWorked(r) {
			c.problems[r.field.Id.String()] = linkedReadWords.Replace(rollupNotWorked)
		}
	}
}

// linkedReadWords puts why a rollup can't add up a field as why a formula
// can't read it.
var linkedReadWords = strings.NewReplacer("A rollup can't add up", "A formula can't read", "this adds up", "this reads", "The relation this reads", "The link this reads")

// planLinkedReads makes each {Link}.{Field} the formulas read a list rollup
// of the field over the link, checked as a rollup is: the read fetches the
// link's rows and their field as it does for one.
func (c *computed) planLinkedReads(ctx context.Context, byID map[string]*model.Field) {
	for _, ref := range c.program.LinkedRefs() {
		link, field, _ := formula.SplitLinkedRef(ref)
		r := &rollupField{
			field: &model.Field{Id: uuid.NewSHA1(uuid.NameSpaceOID, []byte("linked-read:"+ref)), Type: model.FieldRollup},
			cfg:   rollupConfig{Relation: link, Field: field, Aggregate: "list"},
		}
		if why := c.checkRollup(ctx, r, byID); why != "" {
			// What a rollup can't add up, a formula can't read.
			c.problems[r.field.Id.String()] = linkedReadWords.Replace(why)
		} else {
			c.rolledUp[r.link.field.Id.String()] = true
		}
		if c.linkedReads == nil {
			c.linkedReads = map[string]*rollupField{}
		}
		c.linkedReads[ref] = r
	}
}

// liveOnly is fields without those in the trash, in their order: fields
// itself when none is. Pure.
func liveOnly(fields []*model.Field) []*model.Field {
	for i, f := range fields {
		if f.DeletedAt == nil {
			continue
		}
		out := append(make([]*model.Field, 0, len(fields)-1), fields[:i]...)
		for _, g := range fields[i+1:] {
			if g.DeletedAt == nil {
				out = append(out, g)
			}
		}
		return out
	}
	return fields
}

// computedNeeds is the computed fields that reading the fields ids takes, by
// id: those of them that are computed, the formulas, links (to tables' rows
// and to items) and rollups those read, and the relations the rollups add up.
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
		} else if readsLinks(f) || rowStamp(f) != nil {
			keep[id] = true
		}
	}
	for _, id := range ids {
		need(id)
	}
	return keep
}

// rowStamp is how a field worked out from the row itself reads it (its
// number, when it was made and last changed, and by whom), nil for any other.
func rowStamp(f *model.Field) func(*model.Field, *model.Row) any {
	if f == nil {
		return nil
	}
	if k, ok := fieldTypes.Kinds.Lookup(f.Type); ok {
		return k.FromRow
	}
	return nil
}

// stampNamesPeople is whether a field worked out from the row is who made it
// or last changed it: people, whom each read names for its reader, as a link
// to people is.
func stampNamesPeople(f *model.Field) bool {
	return f != nil && (f.Type == model.FieldCreatedBy || f.Type == model.FieldModifiedBy)
}

// workedOutOnRead is whether a field's cells, as a reader sees them, are
// worked out on each read rather than read as stored: formulas, rollups,
// links to tables' rows, links to items (people among them), stored as ids
// and named for the reader, and what is read from the row itself. A query
// that filters, groups or adds up by one works it out first, and never looks
// for it in the stored cells.
func workedOutOnRead(f *model.Field) bool {
	return f != nil && (readsLinks(f) || f.Type == model.FieldFormula || f.Type == model.FieldRollup || rowStamp(f) != nil)
}

// labelField is the field whose value names a table's rows: its primary
// field (migration 300), or, when the primary can't name rows (it links, or
// rolls up), the first with a value of its own, or a formula that doesn't
// read links or rollups.
func labelField(fields []*model.Field) *model.Field {
	for _, f := range fields {
		if f.Primary && namesRows(f, fields) {
			return f
		}
	}
	for _, f := range fields {
		if namesRows(f, fields) {
			return f
		}
	}
	return nil
}

// namesRows is whether a field's value can name a table's rows: one of its
// own, or a formula's that doesn't read links, rollups or who made or changed
// a row, worked out for every reader alike. What is read from the row itself
// (its number, when it was made) doesn't name it.
func namesRows(f *model.Field, fields []*model.Field) bool {
	if readsLinks(f) || f.Type == model.FieldRollup || rowStamp(f) != nil {
		return false
	}
	return f.Type != model.FieldFormula || !readsComputed(f, fields)
}

// target is the table a relation field links to, loaded once a read.
func (c *computed) target(ctx context.Context, id uuid.UUID) *target {
	if t, ok := c.targets[id]; ok {
		return t
	}
	t := newTarget()
	t.opened = *c.open(ctx, id)
	c.targets[id] = t
	c.order = append(c.order, id)
	return t
}

// open is table id as the read's reader opens it, once a read however deep
// its rollups go, so every level sees the same fields.
func (c *computed) open(ctx context.Context, id uuid.UUID) *opened {
	if o, ok := c.tables[id]; ok {
		return o
	}
	o := &opened{}
	c.tables[id] = o
	tb, err := model.GetTableByID(ctx, id)
	if err != nil || tb == nil {
		return o
	}
	o.table, o.visible = tb, mayOpen(ctx, tb, c.own)
	if !o.visible {
		return o
	}
	if id == c.own {
		o.fields = c.fields
	} else if o.fields, err = model.ListFields(ctx, id); err != nil {
		o.visible = false
		return o
	}
	o.label = labelField(o.fields)
	return o
}

// fieldsOf is a table's fields as the read's reader may read them: none
// for one they can't open.
func (c *computed) fieldsOf(ctx context.Context) func(uuid.UUID) []*model.Field {
	return func(id uuid.UUID) []*model.Field {
		if o := c.open(ctx, id); o.visible {
			return o.fields
		}
		return nil
	}
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
	}
	if len(r.cfg.Where) > 0 {
		// Each condition's field is read from the linked rows.
		for _, w := range r.cfg.Where {
			f := resolveField(t.fields, w.Field)
			if f == nil {
				return "A field this rollup's conditions read has been deleted"
			}
			t.reading(f)
		}
		t.conds = append(t.conds, &rollupCond{where: r.cfg.Where})
		r.cond = len(t.conds)
	}
	if r.cfg.Sort != "" {
		// The rows are read, and sorted, by it before they're added up.
		by := resolveField(t.fields, r.cfg.Sort)
		if by == nil {
			return "The field this rollup sorts by has been deleted"
		}
		if why := sortProblem(by, t.fields); why != "" {
			return why
		}
		t.reading(by)
		r.sorted = t.sortingBy(by, r.cfg.Desc)
	}
	if r.cfg.Aggregate == "count" {
		return ""
	}
	if r.cfg.Aggregate == rollupByFormula {
		r.program = rollupProgram(r.cfg.Formula).Within(c.budget)
		if err := r.program.Err(rollupDraftID); err != nil {
			return formula.Message(err)
		}
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
	if r.of.Type == model.FieldRollup {
		fieldsOf := c.fieldsOf(ctx)
		if why := rollupChainProblem(r.field.Id, r.of, t.fields, func(id uuid.UUID) []*model.Field {
			r.chain = append(r.chain, id)
			return fieldsOf(id)
		}); why != "" {
			return why
		}
		t.deeper = append(t.deeper, r.of.Id.String())
	}
	t.reading(r.of)
	return ""
}

// rollupChainProblem is why rollup self can't add up of, a rollup in a table
// with fields, or "": the rollups it would go through, each adding up the
// next, go more than maxRollupDepth deep counting self, or come back round to
// one of them. fieldsOf is a table's fields, none when the reader can't open
// it; a rollup on the way that can't be worked out says why itself.
func rollupChainProblem(self uuid.UUID, of *model.Field, fields []*model.Field, fieldsOf func(uuid.UUID) []*model.Field) string {
	seen := map[uuid.UUID]bool{self: true}
	for depth := 2; ; depth++ {
		if seen[of.Id] {
			return rollupsGoRound
		}
		if depth > maxRollupDepth {
			return rollupsTooDeep
		}
		seen[of.Id] = true
		cfg, _ := rollupOf(of)
		l, ok := linkOf(resolveField(fields, cfg.Relation))
		if !ok {
			return ""
		}
		fields = fieldsOf(l.table)
		if of = resolveField(fields, cfg.Field); of == nil || of.Type != model.FieldRollup {
			return ""
		}
	}
}

// rollupProblem is why a rollup can't add up field of in a table with fields
// that way, or "": another rollup by what it gives, its chain checked apart
// (rollupChainProblem).
func rollupProblem(how string, of *model.Field, fields []*model.Field) string {
	if _, isLink := linkOf(of); isLink {
		return "A rollup can't add up links to another table"
	}
	if _, isItems := itemsOf(of); isItems || stampNamesPeople(of) {
		return "A rollup can't add up links to docs, tasks, boards, projects or people"
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
// formulas, a field linking to a table or to items, a rollup, or who made or
// last changed a row: values a rollup's read of another table doesn't work
// out. A row's number and when it was made or changed it does (target.load).
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
		if readsLinks(g) || g.Type == model.FieldRollup || stampNamesPeople(g) {
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

// apply works out a page of rows' computed cells and splices them in. Each
// row keeps the cells of the table's fields alone: a deleted field's stay
// stored (DeleteField only marks the field), and went to every reader, guests
// and the AI among them.
func (c *computed) apply(ctx context.Context, rows []*model.Row) {
	if c.program.Empty() && len(c.links) == 0 && len(c.rollups) == 0 && len(c.items) == 0 && len(c.stamps) == 0 {
		for _, r := range rows {
			if r != nil {
				r.Values = keepCells(r.Values, c.live)
			}
		}
		return
	}
	if len(rows) == 0 {
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
	c.now, c.loc = now, loc // a rollup's formula reads today where the reader is
	c.readLinks(ctx, ids)
	c.loadLinked(ctx, ids, now, loc)
	// Each row's links to items, by field, as stored; then named, all at once.
	stored := make([][]fieldTypes.ItemRef, len(page)*len(c.items))
	var wanted []fieldTypes.ItemRef
	for i, pr := range page {
		for j, it := range c.items {
			refs, _ := fieldTypes.ParseItems(pr.raw(it.field.Id.String()), it.target)
			stored[i*len(c.items)+j] = refs
			wanted = append(wanted, refs...)
		}
	}
	// The people a row's stamps name (who made it, and last changed it),
	// named with the links, for the person reading.
	stampRefs := make([][]fieldTypes.ItemRef, len(page)*len(c.stamps))
	for i, pr := range page {
		for j, f := range c.stamps {
			if refs, ok := c.stampOf[j](f, pr.row).([]fieldTypes.ItemRef); ok {
				stampRefs[i*len(c.stamps)+j] = refs
				wanted = append(wanted, refs...)
			}
		}
	}
	var names itemNames
	if len(wanted) > 0 {
		names = nameItems(ctx, wanted)
	}
	for i, pr := range page {
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
		for j, it := range c.items {
			fid := it.field.Id.String()
			refs, v := names.cell(stored[i*len(c.items)+j])
			if len(refs) > 0 {
				out[fid] = refs
			} else {
				out[fid] = nil
			}
			values[fid] = v
		}
		for j, f := range c.stamps {
			fid := f.Id.String()
			if refs := stampRefs[i*len(c.stamps)+j]; refs != nil {
				named, v := names.cell(refs)
				if len(named) > 0 {
					out[fid] = named
				} else {
					out[fid] = nil
				}
				values[fid] = v
				continue
			}
			v := c.stampOf[j](f, pr.row)
			out[fid] = v
			values[fid] = cellValue(f.Type, v)
		}
		for _, r := range c.rollups {
			v := c.rollUp(r, row)
			out[r.field.Id.String()] = v.JSON()
			values[r.field.Id.String()] = v
		}
		if !c.program.Empty() {
			worked := c.program.RunRow(formulaRow(pr.row), func(id string) (formula.Value, int) {
				if v, ok := values[id]; ok {
					return v, 0
				}
				if r := c.linkedReads[id]; r != nil {
					return c.rollUp(r, row), 0 // charged to the budget as it adds up
				}
				return pr.cell(id, c.types[id])
			}, now, loc)
			for id, v := range worked {
				out[id] = v.JSON()
			}
		}
		pr.row.Values = withValues(pr.row.Values, pr.cells, out, c.live)
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
				want := min(n, c.shown())
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

// loadLinked reads the linked rows a page needs, up to maxLinkedRows a read:
// first those its cells show, a row of the page at a time across the fields,
// then those rollups add up. The rest are left out (target.skipped).
func (c *computed) loadLinked(ctx context.Context, ids []uuid.UUID, now time.Time, loc *time.Location) {
	wanted := map[uuid.UUID][]uuid.UUID{}
	seen := map[uuid.UUID]bool{}
	for pass := 0; pass < 2; pass++ {
		for _, id := range ids {
			for _, l := range c.links {
				t, pl := c.targets[l.table], c.linked[l.field.Id.String()]
				if pl == nil || !t.visible {
					continue
				}
				of := pl.of[id]
				part := of[:min(len(of), c.shown())]
				if pass == 1 {
					part = of[len(part):]
				}
				for _, other := range part {
					if seen[other] {
						continue
					}
					seen[other] = true
					if c.loaded == maxLinkedRows {
						t.skipped[other] = true
						continue
					}
					c.loaded++
					wanted[l.table] = append(wanted[l.table], other)
				}
			}
		}
	}
	for _, id := range c.order {
		c.load(ctx, c.targets[id], wanted[id], now, loc)
	}
}

// shown is how many of a row's links its cell shows: none one level down,
// where cells are only added up.
func (c *computed) shown() int {
	if c.depth > 0 {
		return 0
	}
	return maxShownLinks
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
		if rowStamp(f) != nil {
			return // read from the row itself, nothing stored
		}
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
	stamps := map[string]*model.Field{}
	for _, f := range t.fields {
		types[f.Id.String()] = f.Type
		if rowStamp(f) != nil && !stampNamesPeople(f) {
			stamps[f.Id.String()] = f
		}
	}
	var program *formula.Program
	if len(formulas) > 0 {
		program = formula.Compile(onlyFormulas(t.fields, formulas)).Within(c.budget)
	}
	// The conditions read the linked rows as the grid's filters do, for no
	// one in particular: a rollup adds up alike for every reader.
	if len(t.conds) > 0 {
		at := reading{zone: loc, now: now, kinds: resultKinds(program, t.reads)}
		for _, rc := range t.conds {
			rc.ready(t.fields, at)
		}
	}
	// The sorts order them as the grid's sorts do, alike for every reader.
	if len(t.sorts) > 0 {
		t.readySorts(resultKinds(program, t.reads))
	}
	// The rollups rollups add up here are worked out for these rows first,
	// one level deeper (planDeeper).
	deeper := make([]*rollupField, len(t.reads))
	if t.sub != nil {
		t.sub.now, t.sub.loc = now, loc
		t.sub.readLinks(ctx, ids)
		t.sub.loadLinked(ctx, ids, now, loc)
		for _, r := range t.sub.rollups {
			if at, ok := t.readAt[r.field.Id.String()]; ok {
				deeper[at] = r
			}
		}
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
			// A cell as the linked row has it: stored, or read from the row
			// itself (its number, when it was made and last changed).
			cell := func(id string) (formula.Value, int) {
				if f := stamps[id]; f != nil {
					return cellValue(f.Type, rowStamp(f)(f, r)), 1
				}
				return pr.cell(id, types[id])
			}
			var worked map[string]formula.Value
			if program != nil {
				worked = program.RunRow(formulaRow(r), cell, now, loc)
			}
			value := func(f *model.Field) formula.Value {
				if f.Type == model.FieldFormula {
					return worked[f.Id.String()]
				}
				v, _ := cell(f.Id.String())
				return v
			}
			lr := &linkedRow{values: make([]formula.Value, len(t.reads))}
			if t.label != nil {
				lr.label = clipLabel(value(t.label).Text())
			}
			for i, f := range t.reads {
				if d := deeper[i]; d != nil {
					lr.values[i] = t.sub.rollUp(d, r.Id)
				} else {
					lr.values[i] = value(f)
				}
			}
			if len(t.conds) > 0 || len(t.sorts) > 0 {
				// The conditions and sorts read cells as the grid's filters
				// and sorts do: raw, a formula as worked out; what the row
				// says of itself (its number, when it was made), from the row.
				raw := map[string]interface{}{}
				for _, f := range t.reads {
					id := f.Id.String()
					if f.Type == model.FieldFormula {
						raw[id] = worked[id].JSON()
					} else {
						raw[id] = pr.raw(id)
					}
				}
				if len(t.conds) > 0 {
					lr.passes = make([]bool, len(t.conds))
					for i, rc := range t.conds {
						lr.passes[i] = rc.passes(r, raw)
					}
				}
				t.key(lr, r, raw)
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
	if r.cond > 0 {
		if err := c.targets[r.link.table].conds[r.cond-1].err; err != nil {
			return formula.Error(err.Error())
		}
	}
	pl := c.linked[r.link.field.Id.String()]
	if pl == nil || pl.failed {
		return errLinksFailed
	}
	n := pl.count[row]
	switch {
	case pl.uncounted[row]:
		return errLinksUncounted
	case r.cfg.Aggregate != "count" || r.cond > 0:
	case r.cfg.Limit > 0 && n > r.cfg.Limit:
		// A count of its first N, whatever their order: no rows to read.
		return formula.Number(float64(r.cfg.Limit))
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
	// The linked rows it adds up, in link order: those that meet its
	// conditions, then sorted and cut to its first N when it says so.
	rows := make([]*linkedRow, 0, len(others))
	for _, id := range others {
		lr, ok := t.rows[id]
		if !ok {
			if t.skipped[id] {
				return errTooManyLinked
			}
			continue // deleted since
		}
		if r.cond > 0 && !lr.passes[r.cond-1] {
			continue
		}
		rows = append(rows, lr)
	}
	if r.sorted > 0 {
		if err := t.sorts[r.sorted-1].err; err != nil {
			return formula.Error(err.Error())
		}
		if !c.sortLinked(rows, r.sorted-1) {
			return formula.RanOut()
		}
	}
	if r.cfg.Limit > 0 && len(rows) > r.cfg.Limit {
		rows = rows[:r.cfg.Limit]
	}
	if r.cfg.Aggregate == "count" {
		return formula.Number(float64(len(rows)))
	}
	how := r.cfg.Aggregate
	if how == rollupByFormula {
		how = "values" // every value, for the formula
	}
	acc := newRollupAcc(how)
	for _, lr := range rows {
		v := t.read(lr, r.of.Id.String())
		if v.IsUnfinished() {
			return v // the linked row's own value left something out
		}
		if !c.budget.Spend(1 + len(v.Str)/32) {
			return formula.RanOut()
		}
		if acc.add(v) {
			break
		}
	}
	switch r.cfg.Aggregate {
	case rollupByFormula:
		values := acc.result()
		if values.Kind == formula.KindError {
			return values
		}
		return r.program.Run(func(string) (formula.Value, int) { return values, 0 }, c.now, c.loc)[rollupDraftID]
	}
	return acc.result()
}

// sortLinked puts a rollup's linked rows in its target's sort at, by their
// keys (rowQuery.Sorter, the tables' one order): rows that tie stay in link
// order. Each comparison is charged to the read's budget; once it runs out
// the rest compare as ties, and false says so.
func (c *computed) sortLinked(rows []*linkedRow, at int) bool {
	enough := true
	sort.SliceStable(rows, func(i, j int) bool {
		if !enough || !c.budget.Spend(1) {
			enough = false
			return false
		}
		return rows[i].keys[at].Compare(rows[j].keys[at]) < 0
	})
	return enough
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
	// items are a list's values, for formulas that read it as a list
	// (ARRAYUNIQUE, COUNTA); its text is what it shows.
	items   []formula.Value
	chars   int
	tooLong bool
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
	case "values":
		if inner := v.Items(); inner != nil {
			a.items = append(a.items, inner...)
		} else {
			a.items = append(a.items, v)
		}
	case "list", "unique":
		s := v.Text()
		if a.how == "unique" {
			if a.seen[s] {
				return false
			}
			a.seen[s] = true
		}
		if len(a.items) > 0 {
			a.chars += 2
		}
		// A linked row's own list (a multi-select) gives its items, as a
		// lookup of it does in Airtable.
		if inner := v.Items(); inner != nil {
			a.items = append(a.items, inner...)
		} else {
			a.items = append(a.items, v)
		}
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
	case "values":
		return formula.List(a.items)
	case "list", "unique":
		if a.tooLong {
			return formula.Error(fmt.Sprintf("The list would be longer than %d characters", maxRollupText))
		}
		return formula.List(a.items)
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

// raw is a stored cell decoded, or nil when the row has none.
func (p *pageRow) raw(id string) interface{} {
	i, ok := p.at[id]
	if !ok {
		return nil
	}
	var v interface{}
	_ = json.Unmarshal([]byte(p.row.Values[p.cells[i].value:p.cells[i].end]), &v)
	return v
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
	// A link's fields by name too ({Vendor}.{City}), from the tables the
	// read loaded.
	inputs := withLinked(formulaInputs(fields), fields, func(table uuid.UUID) []*model.Field {
		if t := c.targets[table]; t != nil && t.visible {
			return t.fields
		}
		return nil
	})
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
