// Package business (QuerySpec) is the Query Spec
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #80; A2 builds it, agreed
// with tables in SHARED_WITH_TABLES.md): one shape for every list of work and
// every table view, filtered, sorted, grouped, counted and paged on the server,
// never from a partial page (PJ10).
//
// A spec names its scope (a project's tasks, my tasks, a table's rows, or the
// whole workspace for search), a filter as nested and/or/not groups at most
// three deep, up to five sorts, up to three group levels, search text and its
// layout's options. Run answers the ids of one page with the groups' counts
// and summaries and the cursor of the next page, through the Runner
// registered for the scope's kind: projects' for tasks (A2), tables' for rows
// (tables A3, its Row Query's JSONB compiler). The search index compiles the
// search scope (docs C2).
//
// The tree is the one typed tree of design/QUERY_STACK_CONTRACT.md (§2), signed
// by the docs, projects and tables leads on 17 Oct 2026. business/SearchQuery
// builds it from the one grammar; every compiler reads only a built spec. The
// operator names below are the one list (§2 item 9); Field Types (#60) says
// which ones each kind of field takes. Dates mean the same in every compiler
// through Value.Window (dates.go).
//
// Validate checks the spec's shape and limits only. No runner is registered
// yet, so Run answers ErrNotBuilt for every scope.
package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// Spec is one query.
type Spec struct {
	Scope  Scope   `json:"scope"`
	Filter *Filter `json:"filter,omitempty"`
	Sorts  []Sort  `json:"sorts,omitempty"`
	Groups []Group `json:"groups,omitempty"`
	// Summaries are the per-field summaries a layout shows (a grid's summary
	// bar, group footers): answered beside each group's count and for the
	// whole set, from the server.
	Summaries []SummaryRequest `json:"summaries,omitempty"`
	// Search is a list's own search box: words, joined to the filter with AND.
	// The grammar puts typed words into the filter instead (as text
	// conditions), so they can take part in OR, NOT and brackets.
	Search string `json:"search,omitempty"`
	// Layout is the view it is for: list, board, timeline, grid, gallery,
	// calendar or form.
	Layout string `json:"layout,omitempty"`
	// Options are the layout's own (a grid's hidden fields, order, widths,
	// frozen count, row height and colour rules; a board's lanes), kept as the
	// layout writes them.
	Options json.RawMessage `json:"options,omitempty"`
}

// Scope is what a spec lists.
type Scope struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	// Kinds are the kinds of thing a search covers (task, doc, message,
	// file, event, row, …); none means every kind. Search scope only.
	Kinds []string `json:"kinds,omitempty"`
}

// The scopes.
const (
	// ScopeProject is a project's tasks (ID: the project).
	ScopeProject = "project"
	// ScopeMine is the tasks assigned to the person asking.
	ScopeMine = "mine"
	// ScopeTable is a table's rows (ID: the table).
	ScopeTable = "table"
	// ScopeSearch is the whole workspace, across kinds (Kinds narrows it).
	ScopeSearch = "search"
)

// Filter is a node of a spec's filter: a group, whose Op ("and", "or" or
// "not") joins its Filters, or a condition on one field.
type Filter struct {
	Op      string   `json:"op,omitempty"`
	Filters []Filter `json:"filters,omitempty"`
	// Field is a built-in name ("status", "assignee", "text" for words) or
	// "field:<id>" for a custom field.
	Field string `json:"field,omitempty"`
	// Operator is one of the names below, never a symbol.
	Operator string `json:"operator,omitempty"`
	// Values is always a list: empty for empty, not_empty, checked and
	// unchecked; two for between.
	Values []Value `json:"values,omitempty"`
	// Form is how the condition was written: FormModifier (from:@sam) or
	// FormCompare (status = done); empty for words. Span is where it sits in
	// the text, [from, to) in UTF-16 code units. Neither changes what a query
	// means, and no compiler reads them: they let a query be written back as
	// it was typed, and point chips and errors at their place.
	Form string `json:"form,omitempty"`
	Span *Span  `json:"span,omitempty"`
}

// Span is [from, to) in UTF-16 code units of the text a query was parsed
// from, as JavaScript counts a string.
type Span [2]int

// The groups' joins. A "not" group holds exactly one filter.
const (
	OpAnd = "and"
	OpOr  = "or"
	OpNot = "not"
)

// How a condition was written.
const (
	FormModifier = "mod"
	FormCompare  = "cmp"
)

// The one list of operator names (design/QUERY_STACK_CONTRACT.md §2 item 9).
// Field Types (#60) says which ones each kind of field takes; the grammar's
// symbols become these (§4.5).
const (
	OperatorIs            = "is"
	OperatorIsNot         = "is_not"
	OperatorContains      = "contains"
	OperatorNotContains   = "not_contains"
	OperatorStartsWith    = "starts_with"
	OperatorEndsWith      = "ends_with"
	OperatorGt            = "gt"
	OperatorGte           = "gte"
	OperatorLt            = "lt"
	OperatorLte           = "lte"
	OperatorBefore        = "before"
	OperatorAfter         = "after"
	OperatorOnOrBefore    = "on_or_before"
	OperatorOnOrAfter     = "on_or_after"
	OperatorBetween       = "between"
	OperatorWithin        = "within"
	OperatorIn            = "in"
	OperatorNotIn         = "not_in"
	OperatorAllOf         = "all_of"
	OperatorIsExactly     = "is_exactly"
	OperatorEmpty         = "empty"
	OperatorNotEmpty      = "not_empty"
	OperatorChecked       = "checked"
	OperatorUnchecked     = "unchecked"
	OperatorTitleContains = "title_contains"
	OperatorNameContains  = "name_contains"
	OperatorStatusGroupIs = "status_group_is"
)

// valueCounts is how many values each operator takes: {at least, at most}.
var valueCounts = map[string][2]int{
	OperatorIs: {1, MaxValues}, OperatorIsNot: {1, MaxValues},
	OperatorContains: {1, 1}, OperatorNotContains: {1, 1}, OperatorStartsWith: {1, 1}, OperatorEndsWith: {1, 1},
	OperatorGt: {1, 1}, OperatorGte: {1, 1}, OperatorLt: {1, 1}, OperatorLte: {1, 1},
	OperatorBefore: {1, 1}, OperatorAfter: {1, 1}, OperatorOnOrBefore: {1, 1}, OperatorOnOrAfter: {1, 1},
	OperatorBetween: {2, 2}, OperatorWithin: {1, 1},
	OperatorIn: {1, MaxValues}, OperatorNotIn: {1, MaxValues}, OperatorAllOf: {1, MaxValues}, OperatorIsExactly: {1, MaxValues},
	OperatorEmpty: {0, 0}, OperatorNotEmpty: {0, 0}, OperatorChecked: {0, 0}, OperatorUnchecked: {0, 0},
	OperatorTitleContains: {1, 1}, OperatorNameContains: {1, 1}, OperatorStatusGroupIs: {1, MaxValues},
}

// Value is one value a condition compares with (design/QUERY_STACK_CONTRACT.md
// §2 item 11). Each part is optional:
//   - Text: a name as typed before it resolves, a text value, a date written
//     as a day, month or year (2026-10-17, 2026-10, 2026), or a reserved
//     value (Me, MyTeam);
//   - ID with Type: what a name resolved to (user, channel, dm, group,
//     project, team, space, doc, table, row, option, cycle, …);
//   - Number: in the field's display unit;
//   - Relative with N: a relative date (the Relative names below);
//   - Label: how it read when the query was built, for display only.
//
// Compilers read ids, numbers, dates and text, never labels.
type Value struct {
	Text     string   `json:"text,omitempty"`
	ID       string   `json:"id,omitempty"`
	Type     string   `json:"type,omitempty"`
	Number   *float64 `json:"number,omitempty"`
	Relative string   `json:"relative,omitempty"`
	N        int      `json:"n,omitempty"`
	Label    string   `json:"label,omitempty"`
}

// empty is a value with nothing in it.
func (v Value) empty() bool {
	return strings.TrimSpace(v.Text) == "" && v.ID == "" && v.Number == nil && v.Relative == ""
}

// The reserved values: the person asking, and anyone in a team they're in.
// They stay as written in a saved view, so "me" is whoever runs it.
const (
	Me     = "@me"
	MyTeam = "@my_team"
)

// Sort is one sort, by a field.
type Sort struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}

// Group is one level of grouping, by a field. A date field's groups are by
// Granularity. Collapsed are the groups of this level the reader closed,
// each the path of keys from the top level down to it, as a JSON list
// (`["Won","2026-10-01"]` at the second level): a window leaves their rows
// out, a count never does.
type Group struct {
	Field       string   `json:"field"`
	Desc        bool     `json:"desc,omitempty"`
	Collapsed   []string `json:"collapsed,omitempty"`
	Granularity string   `json:"granularity,omitempty"`
}

// CollapsedPaths is the level's collapsed groups as paths: level+1 keys
// each, the top level's first.
func (g Group) CollapsedPaths(level int) ([][]string, error) {
	paths := make([][]string, 0, len(g.Collapsed))
	for _, c := range g.Collapsed {
		var path []string
		if err := json.Unmarshal([]byte(c), &path); err != nil || len(path) != level+1 {
			return nil, fmt.Errorf("a collapsed group of %s is a list of %d keys from the top level down, not %s", g.Field, level+1, c)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// The granularities of a date group.
const (
	GranularityDay     = "day"
	GranularityWeek    = "week"
	GranularityMonth   = "month"
	GranularityQuarter = "quarter"
	GranularityYear    = "year"
)

var granularities = map[string]bool{"": true, GranularityDay: true, GranularityWeek: true, GranularityMonth: true, GranularityQuarter: true, GranularityYear: true}

// SummaryRequest is the summaries wanted for one field.
type SummaryRequest struct {
	Field string   `json:"field"`
	Fns   []string `json:"fns"`
}

// The summary functions (design/QUERY_STACK_CONTRACT.md §2 item 8): count,
// empty, filled, pct_empty, pct_filled and unique for any field; sum, average,
// median, min, max and range for numbers; checked and unchecked for
// checkboxes; earliest, latest and range for dates.
var summaryFns = map[string]bool{
	"count": true, "empty": true, "filled": true, "pct_empty": true, "pct_filled": true, "unique": true,
	"sum": true, "average": true, "median": true, "min": true, "max": true, "range": true,
	"checked": true, "unchecked": true,
	"earliest": true, "latest": true,
}

// Summaries are a set's summaries: by field, then by function, each a JSON
// number, or a date for earliest, latest and a date range.
type Summaries map[string]map[string]json.RawMessage

// Cursor is where a page starts: opaque, from the page before.
type Cursor string

// Page asks for one page: its size, after a cursor ("" for the first).
type Page struct {
	Size  int    `json:"size"`
	After Cursor `json:"after,omitempty"`
}

// GroupCount is one group's key (a value per group level), how many it
// holds, and its summaries.
type GroupCount struct {
	Key       []string  `json:"key"`
	Count     int       `json:"count"`
	Summaries Summaries `json:"summaries,omitempty"`
}

// Result is one page: its rows' ids in order (each store reads its own
// objects), every group's count, the next page's cursor ("" at the end), how
// many there are in all, and the whole set's summaries.
type Result struct {
	IDs       []string     `json:"ids"`
	Groups    []GroupCount `json:"groups,omitempty"`
	Next      Cursor       `json:"next,omitempty"`
	Total     int          `json:"total"`
	Summaries Summaries    `json:"summaries,omitempty"`
}

// The limits a spec keeps (SHARED_WITH_TABLES.md, tables' condition 2;
// design/QUERY_STACK_CONTRACT.md §4.2).
const (
	MaxFilterDepth = 3
	MaxSorts       = 5
	MaxGroups      = 3
	// MaxConditions bounds a filter's conditions, all groups together.
	MaxConditions = 50
	// MaxValues bounds the values of one condition.
	MaxValues = 50
	// MaxSummaries bounds the fields a spec asks summaries of.
	MaxSummaries = 50
	// MaxGroupCounts bounds the groups a spec's counts answer, every level's
	// together, and the groups a level holds collapsed.
	MaxGroupCounts = 1000
	// MaxPage is the largest page.
	MaxPage = 500
)

// ErrBadSpec is a spec that breaks its shape or its limits; its message says
// how.
var ErrBadSpec = errors.New("query: that query can't be run")

// ErrNotBuilt is a scope no runner answers yet.
var ErrNotBuilt = errors.New("query: that isn't built yet")

// Validate checks a spec's shape and limits: a scope; a filter of groups and
// conditions at most MaxFilterDepth deep with at most MaxConditions
// conditions, each naming its field, one of the operator names and as many
// values as the operator takes; at most MaxSorts sorts and MaxGroups groups,
// each naming a field; summaries by known functions. Pure.
func (s Spec) Validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrBadSpec, fmt.Sprintf(format, args...))
	}
	switch s.Scope.Kind {
	case ScopeMine, ScopeSearch:
	case ScopeProject, ScopeTable:
		if strings.TrimSpace(s.Scope.ID) == "" {
			return bad("say which %s", s.Scope.Kind)
		}
	default:
		return bad("there's no list called %q", s.Scope.Kind)
	}
	if len(s.Scope.Kinds) > 0 && s.Scope.Kind != ScopeSearch {
		return bad("only a search covers kinds of things")
	}
	if s.Filter != nil {
		conditions := 0
		if err := s.Filter.check(1, &conditions); err != nil {
			return bad("%s", err)
		}
	}
	if len(s.Sorts) > MaxSorts {
		return bad("sort by at most %d fields", MaxSorts)
	}
	for _, o := range s.Sorts {
		if strings.TrimSpace(o.Field) == "" {
			return bad("a sort names its field")
		}
	}
	if len(s.Groups) > MaxGroups {
		return bad("group by at most %d fields", MaxGroups)
	}
	for i, g := range s.Groups {
		if strings.TrimSpace(g.Field) == "" {
			return bad("a group names its field")
		}
		if !granularities[g.Granularity] {
			return bad("a date group is by day, week, month, quarter or year, not %q", g.Granularity)
		}
		if len(g.Collapsed) > MaxGroupCounts {
			return bad("collapse at most %d groups a level", MaxGroupCounts)
		}
		if _, err := g.CollapsedPaths(i); err != nil {
			return bad("%s", err)
		}
	}
	if len(s.Summaries) > MaxSummaries {
		return bad("summarise at most %d fields", MaxSummaries)
	}
	for _, sum := range s.Summaries {
		if strings.TrimSpace(sum.Field) == "" || len(sum.Fns) == 0 {
			return bad("a summary names its field and what to work out")
		}
		for _, fn := range sum.Fns {
			if !summaryFns[fn] {
				return bad("there's no summary called %q", fn)
			}
		}
	}
	return nil
}

// check is f's shape at depth, counting its conditions into n.
func (f Filter) check(depth int, n *int) error {
	if f.Op == "" {
		if strings.TrimSpace(f.Field) == "" || strings.TrimSpace(f.Operator) == "" || len(f.Filters) > 0 {
			return errors.New("a condition names its field and how it compares")
		}
		counts, ok := valueCounts[f.Operator]
		if !ok {
			return fmt.Errorf("there's no operator called %q", f.Operator)
		}
		if len(f.Values) < counts[0] || len(f.Values) > counts[1] {
			if counts[0] == counts[1] {
				return fmt.Errorf("%s takes %d values, not %d", f.Operator, counts[0], len(f.Values))
			}
			return fmt.Errorf("%s takes %d to %d values, not %d", f.Operator, counts[0], counts[1], len(f.Values))
		}
		for _, v := range f.Values {
			if v.empty() {
				return fmt.Errorf("a value of %s is empty", f.Field)
			}
		}
		*n++
		if *n > MaxConditions {
			return fmt.Errorf("a filter has at most %d conditions", MaxConditions)
		}
		return nil
	}
	if f.Op != OpAnd && f.Op != OpOr && f.Op != OpNot {
		return fmt.Errorf("a group joins with and, or or not, not %q", f.Op)
	}
	if depth > MaxFilterDepth {
		return fmt.Errorf("groups go at most %d deep", MaxFilterDepth)
	}
	if f.Field != "" || f.Operator != "" || len(f.Values) > 0 {
		return errors.New("a group holds conditions, it isn't one")
	}
	if f.Op == OpNot && len(f.Filters) != 1 {
		return errors.New("not holds exactly one condition or group")
	}
	for _, child := range f.Filters {
		if err := child.check(depth+1, n); err != nil {
			return err
		}
	}
	return nil
}

// Runner runs specs for one kind of scope. who is the person asking: a runner
// lists only what the Access Resolver lets them see.
type Runner interface {
	Run(ctx context.Context, who accessBusiness.Subject, spec Spec, page Page) (Result, error)
}

// Registry runs every spec through the runner registered for its scope.
type Registry interface {
	Runner
	// Register sets the runner for a kind of scope, once, from its package's
	// init. A second one for a kind is a mistake two packages made in
	// parallel, and panics where it happened.
	Register(kind string, r Runner)
}

// Queries is where every spec is run.
var Queries Registry = &registry{runners: map[string]Runner{}}

type registry struct {
	mu      sync.RWMutex
	runners map[string]Runner
}

func (g *registry) Register(kind string, r Runner) {
	if kind == "" || r == nil {
		panic("query: a runner needs a kind and something to run")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, taken := g.runners[kind]; taken {
		panic(fmt.Sprintf("query: the scope %q has two runners", kind))
	}
	g.runners[kind] = r
}

// Run checks spec and page and runs them through the runner registered for
// the spec's scope.
func (g *registry) Run(ctx context.Context, who accessBusiness.Subject, spec Spec, page Page) (Result, error) {
	if err := spec.Validate(); err != nil {
		return Result{}, err
	}
	if page.Size <= 0 || page.Size > MaxPage {
		return Result{}, fmt.Errorf("%w: a page holds 1 to %d", ErrBadSpec, MaxPage)
	}
	g.mu.RLock()
	r, ok := g.runners[spec.Scope.Kind]
	g.mu.RUnlock()
	if !ok {
		return Result{}, ErrNotBuilt
	}
	return r.Run(ctx, who, spec, page)
}
