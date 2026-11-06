package business

import (
	"strings"
	"testing"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// A first window counts its filter's set first (keepsFew) only where its
// walk would probe the filter, and then along one of the filter's own
// fields' entries: for every pair of a filter and a sort, never by a field
// in particular.
func TestAFilterIsCountedFirstOnlyWhereItsWalkProbesIt(t *testing.T) {
	cols := columns(deals)
	key := func(f *model.Field) string { return "field:" + f.Id.String() }
	is := func(f *model.Field, v string) query.Filter {
		return query.Filter{Field: key(f), Operator: query.OperatorIs, Values: []query.Value{{Text: v}}}
	}
	lo, hi := 500.0, 509.0
	amountIn := query.Filter{Field: key(fAmount), Operator: query.OperatorBetween, Values: []query.Value{{Number: &lo}, {Number: &hi}}}
	dueIn := query.Filter{Field: key(fDue), Operator: query.OperatorBetween, Values: []query.Value{{Text: "2026-03-01"}, {Text: "2026-06-30"}}}
	won, name := is(fStage, "Won"), is(fName, "Acme")
	and := func(fs ...query.Filter) query.Filter { return query.Filter{Op: query.OpAnd, Filters: fs} }
	or := func(fs ...query.Filter) query.Filter { return query.Filter{Op: query.OpOr, Filters: fs} }
	notWon := query.Filter{Field: key(fStage), Operator: query.OperatorIsNot, Values: []query.Value{{Text: "Won"}}}
	by := func(f *model.Field) []query.Sort { return []query.Sort{{Field: key(f)}} }
	tagsAny := *cond(fTags, query.OperatorIn, text("big"), text("new"))
	ownersMe := *cond(fOwners, query.OperatorIn, text(query.Me))
	taxOver := *cond(fTax, query.OperatorGt, query.Value{Number: &lo})

	for _, c := range []struct {
		name    string
		filter  query.Filter
		sorts   []query.Sort
		counted bool         // the walk probes the filter, so the first window counts
		by      *model.Field // the field whose entries it counts; nil: none can be
		cond    string       // the count's condition on the entry starts so
		vals    int          // with this many parameters
	}{
		{"a stage, in the table's order (its value's own walk)", won, nil, false, nil, "", 0},
		{"a due range, by due (the walk is bounded by it)", dueIn, by(fDue), false, nil, "", 0},
		{"an amount range and more of it, by amount", and(amountIn, amountIn), by(fAmount), false, nil, "", 0},
		{"a stage, by due", won, by(fDue), true, fStage, `s.t = $2::text COLLATE "C" AND `, 1},
		{"an amount range, by name", amountIn, by(fName), true, fAmount, "", 2},
		{"a due range and a stage, by amount: the stage's value first", and(dueIn, won), by(fAmount), true, fStage, `s.t = $2::text`, 1},
		{"a due range and an amount range, by name: the first in order", and(dueIn, amountIn), by(fName), true, fDue, "", 0},
		{"a name, by amount", name, by(fAmount), true, fName, `s.t = $2::text`, 1},
		{"a stage or a name, by due: no entry reads an OR", or(won, name), by(fDue), true, nil, "", 0},
		{"not a stage, by due: no entry reads a negation", notWon, by(fDue), true, nil, "", 0},
		{"a range under an OR, in the table's order", or(dueIn, amountIn), nil, true, nil, "", 0},
		// Fields with no index order (sortWalks has no walk for them): their
		// entries are never counted along, even where a condition reads them.
		{"tags, by due: a multi-select has no index order", tagsAny, by(fDue), true, nil, "", 0},
		{"owners, by due: nor has a person", ownersMe, by(fDue), true, nil, "", 0},
		{"a formula over a value, by due: nor has a formula", taxOver, by(fDue), true, nil, "", 0},
		{"tags and an amount range, by name: the amount, past the tags", and(tagsAny, amountIn), by(fName), true, fAmount, "", 2},
	} {
		f := c.filter
		walks, _, err := walksOf(bySorts(c.sorts), &f, cols)
		if err != nil || walks == nil {
			t.Fatalf("%s: walks %+v, %v", c.name, walks, err)
		}
		if probes := !walkedOnly(&f, walks); probes != c.counted {
			t.Errorf("%s: counted first %v, want %v", c.name, probes, c.counted)
		}
		if !c.counted {
			continue
		}
		got, cond, vals := countedBy(&f, cols, asker{zone: time.UTC})
		switch {
		case c.by == nil && got != nil:
			t.Errorf("%s: counts %s's entries (%s), want none", c.name, got.key, cond)
		case c.by != nil && (got == nil || got.key != c.by.Id.String()):
			t.Errorf("%s: counts %v, want %s", c.name, got, c.by.Name)
		case c.by != nil && (!strings.HasPrefix(cond, c.cond) || !strings.Contains(cond, "s.") || strings.Contains(cond, "r.") || (c.vals > 0 && len(vals) != c.vals)):
			t.Errorf("%s: the count's condition %q with %v, want a condition on the entry alone starting %q with %d", c.name, cond, vals, c.cond, c.vals)
		}
	}
}
