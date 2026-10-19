package business

// Format: a built spec written back as text for the person reading it
// (design/QUERY_STACK_CONTRACT.md §4.8): current field names, and names for
// what values point at read again as that person, so a saved view shared with
// someone never shows them the names its owner saw. A value they may not see
// is written as (hidden); the compilers leave it out for them anyway.
//
// The spec becomes a syntax tree, which the canonical formatter writes, so a
// formatted spec reads back as the same spec.

import (
	"context"
	"strconv"
	"strings"

	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
)

// hidden is a value the reader may not see.
const hidden = "(hidden)"

// Format is spec as text for who.
func Format(ctx context.Context, who Asker, spec querySpec.Spec) string {
	fm := &formatter{ctx: ctx, who: who, scope: spec.Scope}
	if provider, ok := Fields.scopeFields(spec.Scope.Kind); ok {
		fm.custom, _ = provider.Fields(ctx, who, spec.Scope)
	}
	var syn Syntax
	if spec.Filter != nil {
		syn.Root = fm.node(*spec.Filter)
	}
	for _, s := range spec.Sorts {
		f, name, quoted := fm.field(s.Field)
		if f == nil {
			name, quoted = s.Field, true
		}
		syn.Sorts = append(syn.Sorts, SortKey{Field: name, Quoted: quoted, Desc: s.Desc})
	}
	return syn.String()
}

type formatter struct {
	ctx    context.Context
	who    Asker
	scope  querySpec.Scope
	custom []Field
}

// field is the field an id names, and how to type it.
func (fm *formatter) field(id string) (*Field, string, bool) {
	if strings.HasPrefix(id, "field:") {
		for i := range fm.custom {
			if fm.custom[i].id() == id {
				return &fm.custom[i], fm.custom[i].Name, true
			}
		}
		return nil, hidden, true
	}
	if f, ok := Fields.lookup(id); ok {
		return &f, f.Name, false
	}
	return nil, id, false
}

// values are a condition's values as the reader reads them.
func (fm *formatter) values(f *Field, vs []querySpec.Value) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, fm.value(f, v))
	}
	return out
}

func (fm *formatter) value(f *Field, v querySpec.Value) string {
	switch {
	case v.ID != "":
		if f == nil || f.Label == nil {
			return hidden
		}
		if label, ok := f.Label(fm.ctx, fm.who, fm.scope, v); ok {
			return label
		}
		return hidden
	case v.Number != nil:
		return strconv.FormatFloat(*v.Number, 'f', -1, 64)
	case v.Relative != "":
		return relativeText(v)
	case v.Text == querySpec.Me:
		return "me"
	}
	return v.Text
}

// relativeText is a relative date as it's typed.
func relativeText(v querySpec.Value) string {
	n := strconv.Itoa(v.N)
	switch v.Relative {
	case querySpec.RelativeDaysAgo:
		return "-" + n + "d"
	case querySpec.RelativeDaysFromNow:
		return "+" + n + "d"
	case querySpec.RelativeMonthsAgo:
		return "-" + n + "m"
	case querySpec.RelativeMonthsFromNow:
		return "+" + n + "m"
	case querySpec.RelativeYearsAgo:
		return "-" + n + "y"
	case querySpec.RelativeYearsFromNow:
		return "+" + n + "y"
	case querySpec.RelativePastDays:
		return "past_" + n + "_days"
	case querySpec.RelativeNextDays:
		return "next_" + n + "_days"
	case querySpec.RelativeWeekday:
		if v.N >= 1 && v.N <= 7 {
			return weekdayNames[v.N-1]
		}
	case querySpec.RelativeMonthName:
		if v.N >= 1 && v.N <= 12 {
			return monthNames[v.N-1]
		}
	}
	return v.Relative
}

var (
	weekdayNames = [7]string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}
	monthNames   = [12]string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}
)

// The symbols an operator is written with, by form.
var modSymbols = map[string]string{
	querySpec.OperatorIs: SymModifier, querySpec.OperatorContains: SymModifier, querySpec.OperatorIn: SymModifier,
	querySpec.OperatorWithin: SymModifier, querySpec.OperatorStatusGroupIs: SymModifier, querySpec.OperatorAllOf: SymModifier,
	querySpec.OperatorLt: SymLt, querySpec.OperatorLte: SymLe, querySpec.OperatorGt: SymGt, querySpec.OperatorGte: SymGe,
	querySpec.OperatorBefore: SymLt, querySpec.OperatorOnOrBefore: SymLe, querySpec.OperatorAfter: SymGt, querySpec.OperatorOnOrAfter: SymGe,
	querySpec.OperatorIsNot: SymNe, querySpec.OperatorNotIn: SymNe,
}

var cmpSymbols = map[string]string{
	querySpec.OperatorIs: SymEq, querySpec.OperatorWithin: SymEq, querySpec.OperatorStatusGroupIs: SymEq,
	querySpec.OperatorIsNot: SymNe, querySpec.OperatorContains: SymContains, querySpec.OperatorNotContains: SymNotContains,
	querySpec.OperatorTitleContains: SymContains, querySpec.OperatorNameContains: SymContains,
	querySpec.OperatorLt: SymLt, querySpec.OperatorLte: SymLe, querySpec.OperatorGt: SymGt, querySpec.OperatorGte: SymGe,
	querySpec.OperatorBefore: SymLt, querySpec.OperatorOnOrBefore: SymLe, querySpec.OperatorAfter: SymGt, querySpec.OperatorOnOrAfter: SymGe,
}

func (fm *formatter) node(f querySpec.Filter) *Node {
	if f.Op != "" {
		kids := make([]*Node, 0, len(f.Filters))
		for _, g := range f.Filters {
			if n := fm.node(g); n != nil {
				kids = append(kids, n)
			}
		}
		if len(kids) == 0 {
			return nil
		}
		if f.Op == querySpec.OpNot {
			return &Node{Kind: NodeNot, Children: kids[:1]}
		}
		if len(kids) == 1 {
			return kids[0]
		}
		return &Node{Kind: f.Op, Children: kids}
	}
	field, name, quoted := fm.field(f.Field)
	values := fm.values(field, f.Values)
	if f.Field == "text" && len(values) == 1 && (f.Operator == querySpec.OperatorContains || f.Operator == querySpec.OperatorNotContains) {
		word := &Node{Kind: NodeWord, Text: values[0]}
		if needsQuote(values[0]) || Parse(values[0]).Root == nil || Parse(values[0]).Root.Kind != NodeWord {
			word = &Node{Kind: NodePhrase, Text: values[0]}
		}
		if f.Operator == querySpec.OperatorNotContains {
			return &Node{Kind: NodeNot, Children: []*Node{word}}
		}
		return word
	}
	clause := func(op string, vs []string, list bool) *Node {
		return &Node{Kind: NodeClause, Field: name, Quoted: quoted, Form: f.Form, Op: op, Values: vs, List: list}
	}
	switch f.Operator {
	case querySpec.OperatorEmpty:
		return &Node{Kind: NodeClause, Field: name, Quoted: quoted, Form: querySpec.FormCompare, Op: SymEmpty}
	case querySpec.OperatorNotEmpty:
		return &Node{Kind: NodeClause, Field: name, Quoted: quoted, Form: querySpec.FormCompare, Op: SymNotEmpty}
	case querySpec.OperatorChecked, querySpec.OperatorUnchecked:
		yes := "yes"
		if f.Operator == querySpec.OperatorUnchecked {
			yes = "no"
		}
		if f.Form == querySpec.FormModifier {
			return clause(SymModifier, []string{yes}, false)
		}
		return clause(SymEq, []string{yes}, false)
	case querySpec.OperatorStartsWith, querySpec.OperatorEndsWith:
		v := values[0] + "*"
		if f.Operator == querySpec.OperatorEndsWith {
			v = "*" + values[0]
		}
		if f.Form == querySpec.FormModifier {
			return clause(SymModifier, []string{v}, false)
		}
		return clause(SymEq, []string{v}, false)
	case querySpec.OperatorBetween:
		v := strings.Join(values, "..")
		if f.Form == querySpec.FormModifier {
			return clause(SymModifier, []string{v}, false)
		}
		return clause(SymEq, []string{v}, false)
	case querySpec.OperatorIsExactly:
		return &Node{Kind: NodeClause, Field: name, Quoted: quoted, Form: querySpec.FormCompare, Op: SymEq, Values: values, List: true}
	}
	if f.Form == querySpec.FormModifier {
		sym, ok := modSymbols[f.Operator]
		if !ok {
			sym = SymModifier
		}
		negated := f.Operator == querySpec.OperatorIsNot || f.Operator == querySpec.OperatorNotIn || f.Operator == querySpec.OperatorNotContains
		if negated {
			sym = SymModifier
		}
		// Several values are the modifier repeated, which reads back as any of
		// them (all of them, for all_of), or as none of them when negated.
		var kids []*Node
		for _, v := range values {
			c := clause(sym, []string{v}, false)
			if negated {
				kids = append(kids, &Node{Kind: NodeNot, Children: []*Node{c}})
			} else {
				kids = append(kids, c)
			}
		}
		if len(kids) == 1 {
			return kids[0]
		}
		return &Node{Kind: NodeAnd, Children: kids}
	}
	switch f.Operator {
	case querySpec.OperatorIn:
		return clause(SymIn, values, true)
	case querySpec.OperatorNotIn:
		return clause(SymNotIn, values, true)
	case querySpec.OperatorAllOf:
		var kids []*Node
		for _, v := range values {
			kids = append(kids, clause(SymEq, []string{v}, false))
		}
		if len(kids) == 1 {
			return kids[0]
		}
		return &Node{Kind: NodeAnd, Children: kids}
	}
	if len(values) > 1 {
		op := SymIn
		if f.Operator == querySpec.OperatorIsNot {
			op = SymNotIn
		}
		return clause(op, values, true)
	}
	sym, ok := cmpSymbols[f.Operator]
	if !ok {
		sym = SymEq
	}
	return clause(sym, values, false)
}
