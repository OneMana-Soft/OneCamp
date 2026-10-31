package business

// Build: a text read as the asker into the one typed tree every compiler
// reads (design/QUERY_STACK_CONTRACT.md §2, §4.3–§4.6). Fields are looked up
// in the registry and the scope's custom fields, symbols become operator
// names by the field's family (§4.5), names resolve through the field's
// resolver as the asker, and negations fold into their operators where one
// says the opposite (-from:@sam is not_in).
//
// Two modes. In a search nothing fails: a field nobody registered, a name
// that finds nothing the asker may open, an operator a field can't take or a
// value that isn't one goes back to being the words typed, and a note says
// so. In a list each of those is an error at its place, and the query isn't
// run. Neither ever says whether something the asker can't open exists.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
)

// Mode is how strictly a text is read.
type Mode int

const (
	// ModeSearch never fails: what can't be read as written is searched as
	// words.
	ModeSearch Mode = iota
	// ModeList refuses: what can't be read is an error, and the query isn't
	// run.
	ModeList
)

// The issues Build finds, beside Parse's.
const (
	IssueUnknownField   = "unknown_field"
	IssueAmbiguousField = "ambiguous_field"
	IssueUnknownValue   = "unknown_value"
	IssueBadOperator    = "bad_operator"
	IssueBadValue       = "bad_value"
	IssueUnsortable     = "unsortable"
	IssueUnavailable    = "unavailable"
	IssueTooManyTerms   = "too_many_conditions"
	IssueTooManyValues  = "too_many_values"
	// IssueInvalid is a built tree its own check refuses: Build's checks come
	// first, so it means a field registered with rules the tree can't hold.
	IssueInvalid = "invalid"
)

// How much an issue matters.
const (
	// LevelError is an issue that stops the query.
	LevelError = "error"
	// LevelNote is said quietly; the query runs.
	LevelNote = "note"
)

// Runs reports whether a query built with these issues may run.
func Runs(issues []Issue) bool {
	for _, is := range issues {
		if is.Level == LevelError {
			return false
		}
	}
	return true
}

// Build reads text as the asker, for scope, in mode: the spec, and what in
// the text couldn't be read as written. The spec may run only when Runs
// says so.
func Build(ctx context.Context, who Asker, text string, scope querySpec.Scope, mode Mode) (querySpec.Spec, []Issue) {
	syn := Parse(text)
	b := &builder{ctx: ctx, who: who, scope: scope, mode: mode, src: newSource(text)}
	for _, is := range syn.Issues {
		b.add(is.Code, is.Span, parseMessage(is.Code), nil)
	}
	if provider, ok := Fields.scopeFields(scope.Kind); ok {
		b.custom, b.customErr = provider.Fields(ctx, who, scope)
	}
	spec := querySpec.Spec{Scope: scope}
	if syn.Root != nil {
		spec.Filter, _ = b.convert(syn.Root)
	}
	for _, k := range syn.Sorts {
		b.sort(&spec, k)
	}
	b.limits(spec.Filter)
	if Runs(b.issues) {
		if err := spec.Validate(); err != nil {
			// Build's own checks come first; this is the tree's last word.
			b.issues = append(b.issues, Issue{Code: IssueInvalid, Level: LevelError, Message: "That query can't be run as it is: " + strings.TrimPrefix(err.Error(), querySpec.ErrBadSpec.Error()+": ")})
		}
	}
	return spec, b.issues
}

type builder struct {
	ctx       context.Context
	who       Asker
	scope     querySpec.Scope
	mode      Mode
	src       source
	custom    []Field
	customErr error
	issues    []Issue
}

// add records an issue at span, at the level the mode gives it.
func (b *builder) add(code string, span querySpec.Span, message string, suggestions []string) {
	b.issues = append(b.issues, Issue{Code: code, Span: span, Level: b.level(code), Message: message, Suggestions: suggestions})
}

// level is how much an issue matters in this mode: in a list, everything;
// in a search, only a text that is past a limit.
func (b *builder) level(code string) string {
	if b.mode == ModeList {
		return LevelError
	}
	switch code {
	case IssueTooLong, IssueTooDeep, IssueTooManySorts, IssueTooManyTerms, IssueTooManyValues:
		return LevelError
	}
	return LevelNote
}

func parseMessage(code string) string {
	switch code {
	case IssueUnclosedQuote:
		return "This quote isn't closed, so it runs to the end."
	case IssueUnclosedParen:
		return "This bracket isn't closed."
	case IssueStrayParen:
		return "This bracket doesn't close anything."
	case IssueEmptyValue:
		return "This needs a value after it."
	case IssueTooDeep:
		return fmt.Sprintf("Brackets can go %d deep at most.", MaxBrackets)
	case IssueTooLong:
		return fmt.Sprintf("A search can be %s characters at most.", "1,000")
	case IssueTooManySorts:
		return fmt.Sprintf("You can sort by %d fields at most.", querySpec.MaxSorts)
	case IssueAfterOrder:
		return "ORDER BY goes at the end, so what follows it was left out."
	case IssueBadList:
		return "A list holds values, not brackets."
	}
	return ""
}

// typed is the text a node was written as.
func (b *builder) typed(span querySpec.Span) string {
	return b.src.text(b.runeAt(span[0]), b.runeAt(span[1]))
}

// runeAt is the rune that starts at a UTF-16 offset.
func (b *builder) runeAt(u int) int {
	return sort.SearchInts(b.src.u16, u)
}

func textCondition(text string, span querySpec.Span) querySpec.Filter {
	s := span
	return querySpec.Filter{Field: "text", Operator: querySpec.OperatorContains, Values: []querySpec.Value{{Text: text}}, Span: &s}
}

// convert is one node as a filter, and for a clause the field it names (for
// merging repeats); nil for a node that says nothing.
func (b *builder) convert(n *Node) (*querySpec.Filter, *Field) {
	switch n.Kind {
	case NodeWord, NodePhrase:
		f := textCondition(n.Text, n.Span)
		return &f, nil
	case NodeClause:
		return b.clause(n)
	case NodeNot:
		child, field := b.convert(n.Children[0])
		if child == nil {
			return nil, nil
		}
		neg := negate(child, n.Span)
		if neg.Op != "" {
			field = nil // a not group: nothing to merge
		}
		return neg, field
	case NodeAnd, NodeOr:
		var kids []querySpec.Filter
		var fields []*Field
		for _, c := range n.Children {
			f, field := b.convert(c)
			if f == nil {
				continue
			}
			if f.Op == n.Kind { // a clause read as words is an and of words
				for _, g := range f.Filters {
					kids, fields = append(kids, g), append(fields, nil)
				}
				continue
			}
			kids, fields = append(kids, *f), append(fields, field)
		}
		if n.Kind == NodeAnd {
			kids = mergeRepeats(kids, fields)
		}
		switch len(kids) {
		case 0:
			return nil, nil
		case 1:
			return &kids[0], nil
		}
		s := n.Span
		return &querySpec.Filter{Op: n.Kind, Filters: kids, Span: &s}, nil
	}
	return nil, nil
}

// inverse is the operator that says the opposite of another.
var inverse = map[string]string{
	querySpec.OperatorIs: querySpec.OperatorIsNot, querySpec.OperatorIsNot: querySpec.OperatorIs,
	querySpec.OperatorContains: querySpec.OperatorNotContains, querySpec.OperatorNotContains: querySpec.OperatorContains,
	querySpec.OperatorIn: querySpec.OperatorNotIn, querySpec.OperatorNotIn: querySpec.OperatorIn,
	querySpec.OperatorEmpty: querySpec.OperatorNotEmpty, querySpec.OperatorNotEmpty: querySpec.OperatorEmpty,
	querySpec.OperatorChecked: querySpec.OperatorUnchecked, querySpec.OperatorUnchecked: querySpec.OperatorChecked,
	querySpec.OperatorBefore: querySpec.OperatorOnOrAfter, querySpec.OperatorOnOrAfter: querySpec.OperatorBefore,
	querySpec.OperatorAfter: querySpec.OperatorOnOrBefore, querySpec.OperatorOnOrBefore: querySpec.OperatorAfter,
	querySpec.OperatorLt: querySpec.OperatorGte, querySpec.OperatorGte: querySpec.OperatorLt,
	querySpec.OperatorGt: querySpec.OperatorLte, querySpec.OperatorLte: querySpec.OperatorGt,
}

// negate is f's opposite: the opposite operator where there is one, the
// group inside a not, or a not around f.
func negate(f *querySpec.Filter, span querySpec.Span) *querySpec.Filter {
	if f.Op == querySpec.OpNot {
		inner := f.Filters[0]
		return &inner
	}
	if f.Op == "" {
		if inv, ok := inverse[f.Operator]; ok {
			g := *f
			g.Operator = inv
			s := span
			g.Span = &s
			return &g
		}
	}
	s := span
	return &querySpec.Filter{Op: querySpec.OpNot, Filters: []querySpec.Filter{*f}, Span: &s}
}

// mergeFamilies are the families whose repeated modifiers join (§4.3).
var mergeFamilies = map[Family]bool{FamilyPlace: true, FamilyKind: true, FamilyOption: true, FamilyPeople: true, FamilyList: true, FamilyRows: true, FamilyItems: true}

// mergeRepeats joins an and's repeated modifiers on one field: any of them
// for a field that holds one value (in:#a in:#b), all of them for one that
// holds several (labels:a labels:b). Compare-form clauses keep strict logic.
func mergeRepeats(kids []querySpec.Filter, fields []*Field) []querySpec.Filter {
	type key struct {
		field string
		neg   bool
	}
	first := map[key]int{}
	var out []querySpec.Filter
	for i, f := range kids {
		field := fields[i]
		pos := f.Operator == querySpec.OperatorIs || f.Operator == querySpec.OperatorIn
		neg := f.Operator == querySpec.OperatorIsNot || f.Operator == querySpec.OperatorNotIn
		if f.Op != "" || f.Form != querySpec.FormModifier || field == nil || !mergeFamilies[field.Family] || !(pos || neg) {
			out = append(out, f)
			continue
		}
		k := key{f.Field, neg}
		at, seen := first[k]
		if !seen {
			first[k] = len(out)
			out = append(out, f)
			continue
		}
		m := &out[at]
		m.Values = appendValues(m.Values, f.Values)
		if m.Span != nil && f.Span != nil {
			s := querySpec.Span{min(m.Span[0], f.Span[0]), max(m.Span[1], f.Span[1])}
			m.Span = &s
		}
		switch {
		case neg && (field.Family == FamilyPlace || field.Family == FamilyKind):
			m.Operator = querySpec.OperatorIsNot
		case neg:
			m.Operator = querySpec.OperatorNotIn
		case field.Many:
			m.Operator = querySpec.OperatorAllOf
		case field.Family == FamilyPlace || field.Family == FamilyKind:
			m.Operator = querySpec.OperatorIs
		default:
			m.Operator = querySpec.OperatorIn
		}
	}
	return out
}

// appendValues adds the values a list doesn't hold yet.
func appendValues(have, more []querySpec.Value) []querySpec.Value {
	for _, v := range more {
		dup := false
		for _, h := range have {
			if h.ID == v.ID && h.Text == v.Text && h.Relative == v.Relative && h.N == v.N {
				dup = true
				break
			}
		}
		if !dup && len(have) < querySpec.MaxValues {
			have = append(have, v)
		}
	}
	return have
}

// field is the field a clause names: a bare name among the built-in fields
// first, then the scope's own; a quoted name among the scope's own first.
func (b *builder) field(name string, quoted bool) (Field, string) {
	custom := func() (Field, string) {
		var found []Field
		for _, f := range b.custom {
			if strings.EqualFold(strings.TrimSpace(f.Name), strings.TrimSpace(name)) {
				found = append(found, f)
			}
		}
		switch len(found) {
		case 0:
			return Field{}, IssueUnknownField
		case 1:
			return found[0], ""
		}
		return Field{}, IssueAmbiguousField
	}
	if quoted {
		if f, problem := custom(); problem != IssueUnknownField {
			return f, problem
		}
		if f, ok := Fields.lookup(name); ok {
			return f, ""
		}
		return Field{}, IssueUnknownField
	}
	if f, ok := Fields.lookup(name); ok {
		return f, ""
	}
	return custom()
}

// clause is a clause as a filter, or what the mode makes of one that can't
// be read.
func (b *builder) clause(n *Node) (*querySpec.Filter, *Field) {
	f, problem := b.field(n.Field, n.Quoted)
	switch problem {
	case IssueUnknownField:
		if b.customErr != nil {
			return b.refuse(n, IssueUnavailable, "This list's fields couldn't be read just now. Try again.")
		}
		return b.refuse(n, IssueUnknownField, fmt.Sprintf("There's no field called “%s”.", n.Field), b.closestFields(n.Field)...)
	case IssueAmbiguousField:
		return b.refuse(n, IssueAmbiguousField, fmt.Sprintf("More than one field is called “%s”. Rename one to tell them apart.", n.Field))
	}
	fixed := ""
	if f.Target != "" {
		if n.Op != SymModifier {
			return b.refuse(n, IssueBadOperator, fmt.Sprintf("Write %s: followed by a day, such as %s:friday.", f.Name, f.Name))
		}
		fixed = f.Fixed
		target, ok := Fields.lookup(f.Target)
		if !ok {
			return b.refuse(n, IssueUnknownField, fmt.Sprintf("There's no field called “%s”.", n.Field))
		}
		f = target
	}
	if n.Op == SymEmpty || n.Op == SymNotEmpty {
		switch f.Family {
		case FamilyCheckbox, FamilyFlag, FamilyKind, FamilyPlace:
			return b.badOperator(n, f)
		}
		op := querySpec.OperatorEmpty
		if n.Op == SymNotEmpty {
			op = querySpec.OperatorNotEmpty
		}
		return b.condition(n, f, op, nil), &f
	}
	if len(n.Values) == 0 {
		return nil, nil // Parse said it needs a value
	}
	if len(n.Values) > querySpec.MaxValues {
		b.add(IssueTooManyValues, n.Span, fmt.Sprintf("A list can hold %d values at most.", querySpec.MaxValues), nil)
		return nil, nil
	}
	switch f.Family {
	case FamilyText:
		return b.textClause(n, f)
	case FamilyNumber:
		return b.numberClause(n, f)
	case FamilyDate:
		return b.dateClause(n, f, fixed)
	case FamilyCheckbox:
		return b.checkboxClause(n, f)
	case FamilyAttachment:
		switch n.Op {
		case SymModifier, SymContains:
			return b.condition(n, f, querySpec.OperatorNameContains, []querySpec.Value{{Text: n.Values[0]}}), &f
		case SymNotContains:
			c := b.condition(n, f, querySpec.OperatorNameContains, []querySpec.Value{{Text: n.Values[0]}})
			return negate(c, n.Span), &f
		}
		return b.badOperator(n, f)
	}
	return b.choiceClause(n, f)
}

// condition is a filter on f.
func (b *builder) condition(n *Node, f Field, operator string, values []querySpec.Value) *querySpec.Filter {
	s := n.Span
	return &querySpec.Filter{Field: f.id(), Operator: operator, Values: values, Form: n.Form, Span: &s}
}

// each is a filter per value, joined by op: an OR of is for in on a family
// that has no in, an AND of is_not for not in.
func (b *builder) each(n *Node, f Field, op, join string, read func(string) (querySpec.Value, error)) (*querySpec.Filter, *Field) {
	var kids []querySpec.Filter
	for _, typed := range n.Values {
		v, err := read(typed)
		if err != nil {
			return b.refuse(n, IssueBadValue, err.Error())
		}
		kids = append(kids, *b.condition(n, f, op, []querySpec.Value{v}))
	}
	if len(kids) == 1 {
		return &kids[0], &f
	}
	s := n.Span
	return &querySpec.Filter{Op: join, Filters: kids, Span: &s}, nil
}

func asText(typed string) (querySpec.Value, error) { return querySpec.Value{Text: typed}, nil }

func (b *builder) textClause(n *Node, f Field) (*querySpec.Filter, *Field) {
	v := n.Values[0]
	switch n.Op {
	case SymModifier, SymEq:
		if n.List {
			return b.badOperator(n, f)
		}
		if op, inner, ok := wildcard(v); ok {
			var values []querySpec.Value
			if op != querySpec.OperatorNotEmpty {
				values = []querySpec.Value{{Text: inner}}
			}
			return b.condition(n, f, op, values), &f
		}
		op := querySpec.OperatorIs
		if n.Op == SymModifier {
			op = querySpec.OperatorContains
		}
		return b.condition(n, f, op, []querySpec.Value{{Text: v}}), &f
	case SymNe:
		if n.List {
			return b.each(n, f, querySpec.OperatorIsNot, querySpec.OpAnd, asText)
		}
		if op, inner, ok := wildcard(v); ok {
			var values []querySpec.Value
			if op != querySpec.OperatorNotEmpty {
				values = []querySpec.Value{{Text: inner}}
			}
			return negate(b.condition(n, f, op, values), n.Span), &f
		}
		return b.condition(n, f, querySpec.OperatorIsNot, []querySpec.Value{{Text: v}}), &f
	case SymContains:
		return b.condition(n, f, querySpec.OperatorContains, []querySpec.Value{{Text: v}}), &f
	case SymNotContains:
		return b.condition(n, f, querySpec.OperatorNotContains, []querySpec.Value{{Text: v}}), &f
	case SymIn:
		return b.each(n, f, querySpec.OperatorIs, querySpec.OpOr, asText)
	case SymNotIn:
		return b.each(n, f, querySpec.OperatorIsNot, querySpec.OpAnd, asText)
	}
	return b.badOperator(n, f)
}

var numberOps = map[string]string{
	SymModifier: querySpec.OperatorIs, SymEq: querySpec.OperatorIs, SymNe: querySpec.OperatorIsNot,
	SymLt: querySpec.OperatorLt, SymLe: querySpec.OperatorLte, SymGt: querySpec.OperatorGt, SymGe: querySpec.OperatorGte,
}

func (b *builder) numberClause(n *Node, f Field) (*querySpec.Filter, *Field) {
	switch n.Op {
	case SymIn:
		return b.each(n, f, querySpec.OperatorIs, querySpec.OpOr, readNumber)
	case SymNotIn:
		return b.each(n, f, querySpec.OperatorIsNot, querySpec.OpAnd, readNumber)
	}
	if n.List {
		return b.badOperator(n, f)
	}
	op, ok := numberOps[n.Op]
	if !ok {
		return b.badOperator(n, f)
	}
	typed := n.Values[0]
	if lo, hi, isRange := splitRange(typed); isRange {
		if op != querySpec.OperatorIs {
			return b.badOperator(n, f)
		}
		a, err1 := readNumber(lo)
		z, err2 := readNumber(hi)
		if err1 != nil || err2 != nil {
			return b.refuse(n, IssueBadValue, fmt.Sprintf("“%s” isn't a range of numbers, such as 1..5.", typed))
		}
		return b.condition(n, f, querySpec.OperatorBetween, []querySpec.Value{a, z}), &f
	}
	v, err := readNumber(typed)
	if err != nil {
		return b.refuse(n, IssueBadValue, err.Error()+".")
	}
	return b.condition(n, f, op, []querySpec.Value{v}), &f
}

var dateOps = map[string]string{
	SymNe: querySpec.OperatorIsNot, SymLt: querySpec.OperatorBefore, SymLe: querySpec.OperatorOnOrBefore,
	SymGt: querySpec.OperatorAfter, SymGe: querySpec.OperatorOnOrAfter,
}

func (b *builder) dateClause(n *Node, f Field, fixed string) (*querySpec.Filter, *Field) {
	switch n.Op {
	case SymIn:
		return b.each(n, f, querySpec.OperatorIs, querySpec.OpOr, readDate)
	case SymNotIn:
		return b.each(n, f, querySpec.OperatorIsNot, querySpec.OpAnd, readDate)
	}
	if n.List {
		return b.badOperator(n, f)
	}
	typed := n.Values[0]
	if lo, hi, isRange := splitRange(typed); isRange {
		if fixed != "" || (n.Op != SymModifier && n.Op != SymEq) {
			return b.badOperator(n, f)
		}
		a, err := readDate(lo)
		if err != nil {
			return b.refuse(n, IssueBadValue, err.Error())
		}
		z, err := readDate(hi)
		if err != nil {
			return b.refuse(n, IssueBadValue, err.Error())
		}
		return b.condition(n, f, querySpec.OperatorBetween, []querySpec.Value{a, z}), &f
	}
	v, err := readDate(typed)
	if err != nil {
		return b.refuse(n, IssueBadValue, err.Error())
	}
	op := fixed
	if op == "" {
		switch n.Op {
		case SymModifier, SymEq:
			op = querySpec.OperatorIs
			if rolling(v) {
				op = querySpec.OperatorWithin
			}
		default:
			var ok bool
			if op, ok = dateOps[n.Op]; !ok {
				return b.badOperator(n, f)
			}
			if op == querySpec.OperatorIsNot && rolling(v) {
				return negate(b.condition(n, f, querySpec.OperatorWithin, []querySpec.Value{v}), n.Span), &f
			}
		}
	}
	return b.condition(n, f, op, []querySpec.Value{v}), &f
}

func (b *builder) checkboxClause(n *Node, f Field) (*querySpec.Filter, *Field) {
	if n.List || (n.Op != SymModifier && n.Op != SymEq && n.Op != SymNe) {
		return b.badOperator(n, f)
	}
	yes, ok := readCheckbox(n.Values[0])
	if !ok {
		return b.refuse(n, IssueBadValue, fmt.Sprintf("Say yes or no for “%s”.", f.Name))
	}
	if n.Op == SymNe {
		yes = !yes
	}
	op := querySpec.OperatorUnchecked
	if yes {
		op = querySpec.OperatorChecked
	}
	return b.condition(n, f, op, nil), &f
}

// choiceClause is a clause on a field whose values name things: options,
// people, linked rows and items, places, flags and kinds.
func (b *builder) choiceClause(n *Node, f Field) (*querySpec.Filter, *Field) {
	if n.Op == SymContains || n.Op == SymNotContains {
		if f.Family != FamilyRows && f.Family != FamilyItems {
			return b.badOperator(n, f)
		}
		c := b.condition(n, f, querySpec.OperatorTitleContains, []querySpec.Value{{Text: n.Values[0]}})
		if n.Op == SymNotContains {
			return negate(c, n.Span), &f
		}
		return c, &f
	}
	ordered := n.Op == SymLt || n.Op == SymLe || n.Op == SymGt || n.Op == SymGe
	if ordered && !(f.Family == FamilyOption && f.Ordered) {
		return b.badOperator(n, f)
	}
	if ordered && n.List {
		return b.badOperator(n, f)
	}
	values, ok := b.resolve(n, f)
	if !ok {
		return b.refused(n)
	}
	positive := n.Op == SymModifier || n.Op == SymEq || n.Op == SymIn
	if !positive && n.Op != SymNe && n.Op != SymNotIn && !ordered {
		return b.badOperator(n, f)
	}
	var op string
	switch {
	case ordered:
		op = numberOps[n.Op]
	case f.Group:
		c := b.condition(n, f, querySpec.OperatorStatusGroupIs, values)
		if !positive {
			return negate(c, n.Span), &f
		}
		return c, &f
	case f.Family == FamilyPlace || f.Family == FamilyKind || f.Family == FamilyFlag:
		op = querySpec.OperatorIs
		if !positive {
			op = querySpec.OperatorIsNot
		}
	case f.Family == FamilyOption:
		switch {
		case n.List && n.Op == SymEq:
			return b.badOperator(n, f)
		case n.Op == SymIn:
			op = querySpec.OperatorIn
		case n.Op == SymNotIn:
			op = querySpec.OperatorNotIn
		case positive:
			op = querySpec.OperatorIs
		default:
			op = querySpec.OperatorIsNot
		}
	default: // list, people, rows, items
		switch {
		case n.List && n.Op == SymEq:
			op = querySpec.OperatorIsExactly
		case positive:
			op = querySpec.OperatorIn
		default:
			op = querySpec.OperatorNotIn
		}
	}
	return b.condition(n, f, op, values), &f
}

// The values a person field takes that name nobody in particular.
var reservedPeople = map[string]querySpec.Value{
	"me": {Text: querySpec.Me, Label: "me"}, "@me": {Text: querySpec.Me, Label: "me"},
	"my_team": {Text: querySpec.MyTeam, Label: "my team"}, "@my_team": {Text: querySpec.MyTeam, Label: "my team"},
}

// resolve is a clause's values as what they name: a field's choices, the
// people nobody needs to look up, or the field's resolver, as the asker.
// false when one of them names nothing the asker may open (said already).
func (b *builder) resolve(n *Node, f Field) ([]querySpec.Value, bool) {
	var out []querySpec.Value
	for _, typed := range n.Values {
		if f.Family == FamilyPeople {
			if v, ok := reservedPeople[strings.ToLower(strings.TrimSpace(typed))]; ok {
				out = appendValues(out, []querySpec.Value{v})
				continue
			}
		}
		switch {
		case len(f.Choices) > 0 || f.Family == FamilyFlag || f.Family == FamilyKind:
			c, ok := choiceOf(f.Choices, strings.TrimSpace(typed))
			if !ok {
				b.add(IssueUnknownValue, n.Span, b.say(fmt.Sprintf("%s:%s isn't something this can look for.", f.Name, typed)), closestChoices(f.Choices, typed))
				return nil, false
			}
			label := c.Label
			if label == "" {
				label = c.Value
			}
			out = appendValues(out, []querySpec.Value{{Text: c.Value, Label: label}})
		case f.Resolve != nil:
			found, err := f.Resolve(b.ctx, b.who, b.scope, typed)
			if err != nil {
				b.add(IssueUnavailable, n.Span, b.say(fmt.Sprintf("“%s” couldn't be looked up just now. Try again.", typed)), nil)
				return nil, false
			}
			if len(found) == 0 {
				b.add(IssueUnknownValue, n.Span, b.say(fmt.Sprintf("Nothing called “%s” that you can open.", typed)), nil)
				return nil, false
			}
			out = appendValues(out, found)
		default:
			out = appendValues(out, []querySpec.Value{{Text: strings.TrimSpace(typed)}})
		}
	}
	return out, true
}

// say is a message as the mode means it: in a search, the clause was
// searched as words instead, said after the message's first sentence.
func (b *builder) say(message string) string {
	if b.mode != ModeSearch {
		return message
	}
	if first, rest, ok := strings.Cut(message, ". "); ok {
		return first + ", so it was searched as words. " + rest
	}
	return strings.TrimSuffix(message, ".") + ", so it was searched as words."
}

func (b *builder) badOperator(n *Node, f Field) (*querySpec.Filter, *Field) {
	sym := n.Op
	switch {
	case n.Op == SymModifier:
		sym = ":"
	case n.List:
		sym += " (…)"
	}
	return b.refuse(n, IssueBadOperator, fmt.Sprintf("“%s” can't be compared with %s.", f.Name, describeSym(sym)))
}

func describeSym(sym string) string {
	switch sym {
	case SymIn:
		return "IN"
	case SymNotIn:
		return "NOT IN"
	case SymEmpty:
		return "IS EMPTY"
	case SymNotEmpty:
		return "IS NOT EMPTY"
	}
	return "“" + sym + "”"
}

// refuse records why a clause can't be read, and gives what the mode makes
// of it: its words in a search, nothing in a list.
func (b *builder) refuse(n *Node, code, message string, suggestions ...string) (*querySpec.Filter, *Field) {
	b.add(code, n.Span, b.say(message), suggestions)
	return b.refused(n)
}

// refused is what a clause that couldn't be read becomes: in a search, the
// words and phrases it was typed as; in a list, nothing (its issue stops the
// query).
func (b *builder) refused(n *Node) (*querySpec.Filter, *Field) {
	if b.mode == ModeList {
		return nil, nil
	}
	start := b.runeAt(n.Span[0])
	sub := newSource(b.typed(n.Span))
	var kids []querySpec.Filter
	toks := lex(sub)
	for i, t := range toks {
		if t.kind != tWord && t.kind != tQuoted {
			continue // operators, brackets and commas say nothing as words
		}
		if strings.TrimSpace(t.text) == "" {
			continue
		}
		// A field's name alone, before the quoted value it was given
		// (in:"Q4 launch"), says nothing as a word; the value does.
		if name, ok := strings.CutSuffix(t.text, ":"); ok && t.kind == tWord && isIdent(name) && i+1 < len(toks) && toks[i+1].kind == tQuoted && toks[i+1].start == t.end {
			continue
		}
		span := querySpec.Span{b.src.u16[start+t.start], b.src.u16[start+t.end]}
		kids = append(kids, textCondition(t.text, span))
	}
	switch len(kids) {
	case 0:
		return nil, nil
	case 1:
		return &kids[0], nil
	}
	s := n.Span
	return &querySpec.Filter{Op: querySpec.OpAnd, Filters: kids, Span: &s}, nil
}

// sort adds an ORDER BY key, if its field may sort.
func (b *builder) sort(spec *querySpec.Spec, k SortKey) {
	f, problem := b.field(k.Field, k.Quoted)
	switch {
	case problem == IssueAmbiguousField:
		b.add(IssueAmbiguousField, k.Span, fmt.Sprintf("More than one field is called “%s”. Rename one to tell them apart.", k.Field), nil)
		return
	case problem != "":
		b.add(IssueUnknownField, k.Span, fmt.Sprintf("There's no field called “%s” to sort by.", k.Field), b.closestFields(k.Field))
		return
	case !f.Sortable || f.Target != "":
		b.add(IssueUnsortable, k.Span, fmt.Sprintf("You can't sort by “%s”.", k.Field), nil)
		return
	}
	if len(spec.Sorts) < querySpec.MaxSorts {
		spec.Sorts = append(spec.Sorts, querySpec.Sort{Field: f.id(), Desc: k.Desc})
	}
}

// limits checks the built filter against the tree's limits, saying where it
// passes them.
func (b *builder) limits(f *querySpec.Filter) {
	if f == nil {
		return
	}
	conditions := 0
	var walk func(g *querySpec.Filter, depth int) bool
	walk = func(g *querySpec.Filter, depth int) bool {
		if g.Op == "" {
			conditions++
			if conditions == querySpec.MaxConditions+1 && g.Span != nil {
				b.add(IssueTooManyTerms, *g.Span, fmt.Sprintf("A search can hold %d terms at most.", querySpec.MaxConditions), nil)
			}
			return true
		}
		if depth > querySpec.MaxFilterDepth {
			span := querySpec.Span{}
			if g.Span != nil {
				span = *g.Span
			}
			b.add(IssueTooDeep, span, fmt.Sprintf("Groups can go %d deep at most. Take out a pair of brackets, or join terms with AND.", querySpec.MaxFilterDepth), nil)
			return false
		}
		for i := range g.Filters {
			if !walk(&g.Filters[i], depth+1) {
				return false
			}
		}
		return true
	}
	walk(f, 1)
}

// closestFields are the field names closest to a mistyped one.
func (b *builder) closestFields(typed string) []string {
	names := Fields.names()
	for _, f := range b.custom {
		names = append(names, f.Name)
	}
	return closest(typed, names)
}

func closestChoices(choices []Choice, typed string) []string {
	var names []string
	for _, c := range choices {
		names = append(names, c.Value)
	}
	return closest(typed, names)
}

// closest are up to three names within two edits of typed, or starting with
// it, nearest first.
func closest(typed string, names []string) []string {
	t := strings.ToLower(strings.TrimSpace(typed))
	type scored struct {
		name string
		d    int
	}
	var found []scored
	seen := map[string]bool{}
	for _, name := range names {
		n := strings.ToLower(name)
		if seen[n] || n == t {
			continue
		}
		seen[n] = true
		d := editDistance(t, n)
		if d <= 2 || (len(t) >= 2 && strings.HasPrefix(n, t)) {
			found = append(found, scored{name, d})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].d < found[j].d })
	var out []string
	for i := 0; i < len(found) && i < 3; i++ {
		out = append(out, found[i].name)
	}
	return out
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
