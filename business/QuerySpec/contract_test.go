package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

func cond(field string) Filter {
	return Filter{Field: field, Operator: OperatorIs, Values: []Value{{Text: "x"}}}
}

func group(op string, fs ...Filter) Filter { return Filter{Op: op, Filters: fs} }

// A spec keeps its shape and limits: a scope, groups three deep, at most five
// sorts and three groups, conditions that name their field.
func TestASpecKeepsItsShapeAndLimits(t *testing.T) {
	ok := Spec{Scope: Scope{Kind: ScopeProject, ID: "p"},
		Filter: &Filter{Op: OpAnd, Filters: []Filter{cond("status"), group(OpOr, cond("assignee"), group(OpAnd, cond("due"), cond("label")))}},
		Sorts:  []Sort{{Field: "due"}, {Field: "priority", Desc: true}},
		Groups: []Group{{Field: "status"}, {Field: "assignee"}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a well-made spec: %v", err)
	}
	if err := (Spec{Scope: Scope{Kind: ScopeMine}}).Validate(); err != nil {
		t.Errorf("my tasks need no id: %v", err)
	}
	tooDeep := group(OpAnd, group(OpOr, group(OpAnd, group(OpOr, cond("x")))))
	many := Filter{Op: OpAnd}
	for i := 0; i <= MaxConditions; i++ {
		many.Filters = append(many.Filters, cond("x"))
	}
	for name, s := range map[string]Spec{
		"no scope":                      {},
		"a project with no id":          {Scope: Scope{Kind: ScopeProject}},
		"an unknown list":               {Scope: Scope{Kind: "everything"}},
		"groups four deep":              {Scope: Scope{Kind: ScopeMine}, Filter: &tooDeep},
		"too many conditions":           {Scope: Scope{Kind: ScopeMine}, Filter: &many},
		"a join that isn't":             {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Op: "xor", Filters: []Filter{cond("x")}}},
		"a condition without its field": {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Operator: "is"}},
		"a group that compares":         {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Op: OpAnd, Field: "status", Operator: "is"}},
		"six sorts":                     {Scope: Scope{Kind: ScopeMine}, Sorts: make([]Sort, 6)},
		"a sort with no field":          {Scope: Scope{Kind: ScopeMine}, Sorts: []Sort{{}}},
		"four groups":                   {Scope: Scope{Kind: ScopeMine}, Groups: []Group{{Field: "a"}, {Field: "b"}, {Field: "c"}, {Field: "d"}}},
	} {
		if err := s.Validate(); !errors.Is(err, ErrBadSpec) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// No runner answers yet: Run checks the spec and the page, then says it isn't
// built. A registered runner answers for its scope, once.
func TestRunAnswersThroughTheScopesRunner(t *testing.T) {
	ctx := context.Background()
	who := accessBusiness.Subject{UserUUID: "someone"}
	if _, err := Queries.Run(ctx, who, Spec{Scope: Scope{Kind: ScopeTable, ID: "t"}}, Page{Size: 50}); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("no runner: %v", err)
	}
	if _, err := Queries.Run(ctx, who, Spec{Scope: Scope{Kind: ScopeTable, ID: "t"}}, Page{Size: MaxPage + 1}); !errors.Is(err, ErrBadSpec) {
		t.Errorf("an oversized page: %v", err)
	}
	if _, err := Queries.Run(ctx, who, Spec{}, Page{Size: 50}); !errors.Is(err, ErrBadSpec) {
		t.Errorf("a bad spec: %v", err)
	}

	saved := Queries
	t.Cleanup(func() { Queries = saved })
	Queries = &registry{runners: map[string]Runner{}}
	Queries.Register(ScopeMine, fixed{ids: []string{"a", "b"}})
	got, err := Queries.Run(ctx, who, Spec{Scope: Scope{Kind: ScopeMine}}, Page{Size: 2})
	if err != nil || len(got.IDs) != 2 || got.Total != 2 {
		t.Errorf("a registered runner: %+v %v", got, err)
	}
	defer func() {
		if recover() == nil {
			t.Error("a second runner for one scope must panic")
		}
	}()
	Queries.Register(ScopeMine, fixed{})
}

type fixed struct{ ids []string }

func (f fixed) Run(context.Context, accessBusiness.Subject, Spec, Page) (Result, error) {
	return Result{IDs: f.ids, Total: len(f.ids)}, nil
}

// The tree's additions (design/QUERY_STACK_CONTRACT.md §2): a search across
// kinds, a not group of one, operators from the one list with as many values
// as each takes, date groups by a granularity, and summaries by known
// functions.
func TestTheTreeKeepsTheContractsShape(t *testing.T) {
	two := func(op string, vs ...Value) Filter { return Filter{Field: "due", Operator: op, Values: vs} }
	day := Value{Text: "2026-10-17"}
	ok := []Spec{
		{Scope: Scope{Kind: ScopeSearch}},
		{Scope: Scope{Kind: ScopeSearch, Kinds: []string{"doc", "task"}}, Filter: &Filter{Op: OpNot, Filters: []Filter{cond("from")}}},
		{Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Op: OpAnd, Filters: []Filter{two(OperatorBetween, day, Value{Relative: RelativeToday}), {Field: "due", Operator: OperatorEmpty}}}},
		{Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Field: "assignee", Operator: OperatorIn, Values: []Value{{Text: Me}, {Text: MyTeam}}}},
		{Scope: Scope{Kind: ScopeTable, ID: "t"}, Groups: []Group{{Field: "due", Granularity: GranularityWeek}}, Summaries: []SummaryRequest{{Field: "budget", Fns: []string{"sum", "average"}}, {Field: "due", Fns: []string{"earliest", "range"}}}},
	}
	for i, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("well made %d: %v", i, err)
		}
	}
	for name, s := range map[string]Spec{
		"kinds outside a search": {Scope: Scope{Kind: ScopeMine, Kinds: []string{"doc"}}},
		"not with two children":  {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Op: OpNot, Filters: []Filter{cond("a"), cond("b")}}},
		"not with none":          {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Op: OpNot}},
		"a symbol, not a name":   {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Field: "due", Operator: "<", Values: []Value{day}}},
		"between with one value": {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Field: "due", Operator: OperatorBetween, Values: []Value{day}}},
		"empty with a value":     {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Field: "due", Operator: OperatorEmpty, Values: []Value{day}}},
		"is with no value":       {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Field: "status", Operator: OperatorIs}},
		"an empty value":         {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Field: "status", Operator: OperatorIs, Values: []Value{{Label: "Done"}}}},
		"a group with values":    {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Op: OpAnd, Values: []Value{day}}},
		"a group by the hour":    {Scope: Scope{Kind: ScopeMine}, Groups: []Group{{Field: "due", Granularity: "hour"}}},
		"a summary nobody knows": {Scope: Scope{Kind: ScopeMine}, Summaries: []SummaryRequest{{Field: "x", Fns: []string{"mode"}}}},
		"a summary of nothing":   {Scope: Scope{Kind: ScopeMine}, Summaries: []SummaryRequest{{Field: "x"}}},
		"too many values in one": {Scope: Scope{Kind: ScopeMine}, Filter: &Filter{Field: "label", Operator: OperatorIn, Values: make([]Value, MaxValues+1)}},
	} {
		if err := s.Validate(); !errors.Is(err, ErrBadSpec) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A collapsed group is a path of keys from the top level down to its own: a
// JSON list of as many keys as its level is deep, at most MaxGroupCounts of
// them a level.
func TestACollapsedGroupNamesItsPath(t *testing.T) {
	table := Scope{Kind: ScopeTable, ID: "t"}
	ok := Spec{Scope: table, Groups: []Group{
		{Field: "stage", Collapsed: []string{`["Won"]`, `[""]`}},
		{Field: "due", Granularity: GranularityWeek, Collapsed: []string{`["Won","2026-10-19"]`}},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("well made: %v", err)
	}
	tooMany := make([]string, 1001)
	for i := range tooMany {
		tooMany[i] = `["x"]`
	}
	for name, groups := range map[string][]Group{
		"a key, not a path":          {{Field: "stage", Collapsed: []string{"Won"}}},
		"a path too short":           {{Field: "stage"}, {Field: "due", Collapsed: []string{`["Won"]`}}},
		"a path too long":            {{Field: "stage", Collapsed: []string{`["Won","2026-10-19"]`}}},
		"a key that isn't text":      {{Field: "amount", Collapsed: []string{`[12]`}}},
		"a path that isn't a list":   {{Field: "stage", Collapsed: []string{`{"key":"Won"}`}}},
		"more than a thousand paths": {{Field: "stage", Collapsed: tooMany}},
	} {
		if err := (Spec{Scope: table, Groups: groups}).Validate(); !errors.Is(err, ErrBadSpec) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
