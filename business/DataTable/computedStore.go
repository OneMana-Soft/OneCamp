package business

// The Computed Store's two calls (airtable-03): Serves, the gate deciding
// whether a kept formula or rollup value may answer a reader, and Work, the
// values the store keeps, worked out with every table open. business/RowQuery
// keeps them in the cell index and asks the gate before it answers anyone by
// them, so a kept value never orders, keeps, counts, groups or adds up rows a
// reader's own read wouldn't: a reader who can't open a table it reaches, at
// any level, is answered by their own read (or refused past what one read
// works out), never by the kept value.

import (
	"bytes"
	"context"
	"slices"

	computedStore "github.com/akashc777/OneCamp/business/ComputedStore"
	formula "github.com/akashc777/OneCamp/business/Formula"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// computedValues is the store: read time for a read's cells (readTime), and
// the gate and the fill for what business/RowQuery keeps.
type computedValues struct{ readTime }

// Serves plans the fields ids name as who's own read would (asViewer: who's
// opens, an agent's second person included through ctx), and answers by them
// only when each field is kept (keeps) and every table its values reach, at
// every level (reachOf: the one reach the store also looks at a field again
// by), is one who opens. A table the plan opened for anything else doesn't
// count: a formula's plan opens every table its table links to, in case it
// reads {Link}.{Field}, so one that reads only its row's own cells reaches
// none of them. Opening a table that fails counts as not opened
// (computed.open), as does a table the plan never opened, so a failure is a
// no.
func (computedValues) Serves(ctx context.Context, who tableAccess.Who, table uuid.UUID, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	fields, err := model.LiveFields(ctx, table)
	if err != nil {
		return nil, err
	}
	c := planComputedFor(asViewer(ctx, actorOf(who)), fields, computedNeeds(fields, ids))
	kinds, reach := keptKinds(c, fields), c.reachFn()
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		k, ok := kinds[id]
		if !ok || !c.opensEvery(reach(id)) {
			return nil, nil
		}
		out[id] = k
	}
	return out, nil
}

// Work works rows out with every table open (an admin's read, which opens
// every table not in the trash), and says what each kept field gives. With
// no rows it says instead the tables each formula's and rollup's values
// reach, kept or not (reaches, from the one plan of the table), so a change
// in any of them has the store look at the field again.
func (computedValues) Work(ctx context.Context, table uuid.UUID, ids []uuid.UUID) (*computedStore.Worked, error) {
	ctx = asViewer(ctx, Actor{IsAdmin: true})
	fields, err := model.LiveFields(ctx, table)
	if err != nil {
		return nil, err
	}
	c := planComputed(ctx, fields)
	w := &computedStore.Worked{Kinds: keptKinds(c, fields), Reaches: map[string][]uuid.UUID{}}
	if len(ids) == 0 {
		w.Reaches = c.reaches()
		return w, nil
	}
	if len(w.Kinds) == 0 {
		return w, nil
	}
	rows, err := model.RowsForIndex(ctx, table, ids)
	if err != nil {
		return nil, err
	}
	c.apply(ctx, rows)
	w.Rows = rows
	return w, nil
}

// reaches is the tables each of the plan's formulas and rollups reaches, by
// id, kept or not, from this one plan of their table, with no store call: a
// link it reads reaches the table it links to; a rollup, or a {Link}.{Field}
// a formula reads (a list rollup), reaches that table and, adding up a
// rollup, each table down that chain; a formula reaches what each field it
// reads reaches. A formula that can't be read reaches every table its table
// links to, since any of them may come to hold what it names.
func (c *computed) reaches() map[string][]uuid.UUID {
	reach := c.reachFn()
	out := map[string][]uuid.UUID{}
	for _, f := range c.fields {
		if f.Type != model.FieldFormula && f.Type != model.FieldRollup {
			continue
		}
		into := reach(f.Id.String())
		tables := make([]uuid.UUID, 0, len(into))
		for t := range into {
			tables = append(tables, t)
		}
		slices.SortFunc(tables, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		out[f.Id.String()] = tables
	}
	return out
}

// reachFn is the tables one of the plan's fields, by id, reaches (reachOf),
// over this plan's fields and rollups: the one reach both the store's fill
// (reaches) and its gate (Serves) go by.
func (c *computed) reachFn() func(id string) map[uuid.UUID]bool {
	byID := make(map[string]*model.Field, len(c.fields))
	for _, f := range c.fields {
		byID[f.Id.String()] = f
	}
	rollups := make(map[string]*rollupField, len(c.rollups))
	for _, r := range c.rollups {
		rollups[r.field.Id.String()] = r
	}
	return func(id string) map[uuid.UUID]bool {
		into := map[uuid.UUID]bool{}
		c.reachOf(id, byID, rollups, into, map[string]bool{})
		return into
	}
}

// reachOf adds to into the tables ref reaches: one of the plan's fields, by
// id, or a {Link}.{Field} its formulas read. seen stops a loop.
func (c *computed) reachOf(ref string, byID map[string]*model.Field, rollups map[string]*rollupField, into map[uuid.UUID]bool, seen map[string]bool) {
	if seen[ref] {
		return
	}
	seen[ref] = true
	if link, _, linked := formula.SplitLinkedRef(ref); linked {
		if r := c.linkedReads[ref]; r != nil {
			rollupReach(r, into)
		} else if l, ok := linkOf(byID[link]); ok {
			// No planned read: its link's table at least, so the gate fails
			// closed, never open (CR-92 S1).
			into[l.table] = true
		}
		return
	}
	f := byID[ref]
	if f == nil {
		return
	}
	if l, ok := linkOf(f); ok {
		into[l.table] = true
		return
	}
	switch f.Type {
	case model.FieldRollup:
		if cfg, ok := rollupOf(f); ok {
			c.reachOf(cfg.Relation, byID, rollups, into, seen)
		}
		if r := rollups[ref]; r != nil {
			rollupReach(r, into)
		}
	case model.FieldFormula:
		reads, ok := c.program.Reads(ref)
		if !ok {
			for _, g := range c.fields {
				if l, ok := linkOf(g); ok {
					into[l.table] = true
				}
			}
			return
		}
		for _, read := range reads {
			c.reachOf(read, byID, rollups, into, seen)
		}
	}
}

// rollupReach adds to into the table rollup r adds up and, when it adds up a
// rollup, each table down that chain, as checkRollup went down it (chain).
func rollupReach(r *rollupField, into map[uuid.UUID]bool) {
	if r.link.table == uuid.Nil {
		return // its relation is gone
	}
	into[r.link.table] = true
	for _, t := range r.chain {
		into[t] = true
	}
}

// opensEvery is whether each of tables is there and open to the plan's
// reader, as the plan opened it at any level; one it never opened counts as
// not opened.
func (c *computed) opensEvery(tables map[uuid.UUID]bool) bool {
	for id := range tables {
		if o := c.reach.tables[id]; o == nil || o.table == nil || !o.visible {
			return false
		}
	}
	return true
}

// keptKinds is what each formula and rollup of fields the store keeps gives,
// by id: those that can be worked out (no problem), give a number, a text or
// a checkbox, and are steady (keeps).
func keptKinds(c *computed, fields []*model.Field) map[string]string {
	byID := make(map[string]*model.Field, len(fields))
	for _, f := range fields {
		byID[f.Id.String()] = f
	}
	results := resultKinds(c.program, fields)
	out := map[string]string{}
	for id, k := range results {
		if k != "number" && k != "text" && k != "checkbox" {
			continue
		}
		if c.problems[id] == "" && c.keeps(id, byID, map[string]bool{}) {
			out[id] = k
		}
	}
	return out
}

// keeps is whether a formula or rollup gives the same value whoever reads it
// and whenever: a formula steady by formula.Steady, over fields that are; a
// rollup adding up no date, with no formula or conditions of its own (they
// may read the moment or the reader), over a stored field or a kept rollup.
// Neither is over a link to items each reader names (namedPerReader).
func (c *computed) keeps(id string, byID map[string]*model.Field, seen map[string]bool) bool {
	f := byID[id]
	if f == nil || seen[id] {
		return false
	}
	seen[id] = true
	switch f.Type {
	case model.FieldFormula:
		return c.program.Steady(id, func(ref string) bool {
			if _, _, linked := formula.SplitLinkedRef(ref); linked {
				r := c.linkedReads[ref]
				return r != nil && c.rollupKept(r)
			}
			g := byID[ref]
			if g == nil {
				return false
			}
			if namedPerReader(g) {
				return false
			}
			if g.Type == model.FieldRollup || g.Type == model.FieldFormula {
				return c.keeps(ref, byID, seen)
			}
			return kindOfField(g) != formula.KindDate
		})
	case model.FieldRollup:
		for _, r := range c.rollups {
			if r.field.Id == f.Id {
				return c.rollupKept(r)
			}
		}
	}
	return false
}

// rollupKept is keeps for a rollup: one level down, a rollup it adds up is
// planned in its target's read (target.sub).
func (c *computed) rollupKept(r *rollupField) bool {
	if c.problems[r.field.Id.String()] != "" || r.program != nil || r.cond > 0 || kindOfField(r.field) == formula.KindDate {
		return false
	}
	if r.of == nil {
		return true // a count of links
	}
	if namedPerReader(r.of) {
		return false
	}
	if kindOfField(r.of) == formula.KindDate {
		return false
	}
	switch r.of.Type {
	case model.FieldFormula:
		return false
	case model.FieldRollup:
		t := c.targets[r.link.table]
		if t == nil || t.sub == nil {
			return false
		}
		for _, sr := range t.sub.rollups {
			if sr.field.Id == r.of.Id {
				return t.sub.rollupKept(sr)
			}
		}
		return false
	}
	return true
}
