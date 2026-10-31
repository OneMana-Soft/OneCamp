package business

// The index compiler (design/QUERY_STACK_CONTRACT.md §1): a built spec
// (business/QuerySpec) as one query per source. Each condition is, for a
// source, true of every entry, true of none, or a query: a field a source
// doesn't have can't hold for its entries (from:@sam finds no channels), and
// negated it holds for all of them. A source a condition rules out is left
// out of the search.
//
// Words score; every other condition only filters (a constant score of
// nought), so a filter never reorders what the words found.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	searchQuery "github.com/akashc777/OneCamp/business/SearchQuery"
	teamDomain "github.com/akashc777/OneCamp/domain/Team"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// placeConversation is a place value naming a DM or a group by its grouping
// id, as the search in a conversation's header names it.
const placeConversation = "conversation"

// part is a condition compiled for one source.
type part struct {
	q      map[string]any
	always bool
	never  bool
}

var (
	partAlways = part{always: true}
	partNever  = part{never: true}
)

func query(q map[string]any) part { return part{q: q} }

// filterOnly is q as a condition that filters and adds nothing to the score.
func filterOnly(q map[string]any) part {
	return part{q: map[string]any{"constant_score": map[string]any{"filter": q, "boost": 0}}}
}

func not(p part) part {
	switch {
	case p.always:
		return partNever
	case p.never:
		return partAlways
	}
	return query(map[string]any{"bool": map[string]any{"must_not": []map[string]any{p.q}}})
}

func and(parts []part) part {
	var qs []map[string]any
	for _, p := range parts {
		if p.never {
			return partNever
		}
		if !p.always {
			qs = append(qs, p.q)
		}
	}
	switch len(qs) {
	case 0:
		return partAlways
	case 1:
		return query(qs[0])
	}
	return query(map[string]any{"bool": map[string]any{"must": qs}})
}

func or(parts []part) part {
	var qs []map[string]any
	for _, p := range parts {
		if p.always {
			return partAlways
		}
		if !p.never {
			qs = append(qs, p.q)
		}
	}
	switch len(qs) {
	case 0:
		return partNever
	case 1:
		return query(qs[0])
	}
	return query(map[string]any{"bool": map[string]any{"should": qs, "minimum_should_match": 1}})
}

// compiler compiles one spec for one asker.
type compiler struct {
	ctx   context.Context
	reach Reach
	now   time.Time
	zone  *time.Location
	// lastWord is where the last word ends: the one being typed, which also
	// matches as the start of a title.
	lastWord int
	// allStop is a query of nothing but common words, which then count.
	allStop bool
	// unfiltered are the fields the index can't filter on yet, said once each.
	unfiltered map[string]bool
	// partial are the fields some indices on this server can't filter on (an
	// index that didn't take a searchable copy), whose entries there are left
	// out rather than shown unfiltered.
	partial map[string]bool
	// mates are the people in the asker's teams, read once, for @my_team.
	mates    []string
	matesErr error
	matesSet bool
}

// teamMates are the people in these teams, a seam for tests.
var teamMates = func(ctx context.Context, userDgraphUID string, teamUUIDs []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, t := range teamUUIDs {
		team, err := teamDomain.GetDgraphTeamMemberListByTeamUUID(ctx, t, userDgraphUID)
		if err != nil {
			return nil, err
		}
		if team == nil {
			continue
		}
		for _, m := range append(append([]*dgraphStruct.DgraphUser(nil), team.Members...), team.Admins...) {
			if m != nil && m.Uuid != "" && !seen[m.Uuid] {
				seen[m.Uuid] = true
				out = append(out, m.Uuid)
			}
		}
	}
	return out, nil
}

func newCompiler(ctx context.Context, reach Reach, spec querySpec.Spec, now time.Time, zone *time.Location) *compiler {
	c := &compiler{ctx: ctx, reach: reach, now: now, zone: zone, unfiltered: map[string]bool{}, partial: map[string]bool{}}
	words, stops := 0, 0
	var walk func(f *querySpec.Filter)
	walk = func(f *querySpec.Filter) {
		if f == nil {
			return
		}
		if f.Op != "" {
			for i := range f.Filters {
				walk(&f.Filters[i])
			}
			return
		}
		if f.Field == "text" && len(f.Values) == 1 {
			words++
			if stopWord(f.Values[0].Text) {
				stops++
			}
			if f.Span != nil && f.Span[1] > c.lastWord {
				c.lastWord = f.Span[1]
			}
		}
	}
	walk(spec.Filter)
	c.allStop = words > 0 && words == stops
	return c
}

// fieldReady is the index field when this server's index can filter on it;
// otherwise "", and the field is said to be partly filtered.
var fieldReady = opensearchInit.QueryFieldReady

func (c *compiler) ready(s source, name string, fields []string) []string {
	var out []string
	for _, f := range fields {
		if fieldReady(s.index, f) {
			out = append(out, f)
		} else {
			c.partial[name] = true
		}
	}
	return out
}

// The words a search ignores: so common that requiring them leaves out what
// was meant ("terms and conditions" finds "Terms & Conditions").
var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "but": true, "by": true,
	"for": true, "if": true, "in": true, "into": true, "is": true, "it": true, "of": true, "on": true, "or": true,
	"so": true, "the": true, "their": true, "then": true, "there": true, "these": true, "they": true, "this": true,
	"to": true, "was": true, "will": true, "with": true,
}

func stopWord(w string) bool { return stopWords[strings.ToLower(strings.TrimSpace(w))] }

// compile is spec's filter for one source.
func (c *compiler) compile(s source, f *querySpec.Filter) part {
	if f == nil {
		return partAlways
	}
	switch f.Op {
	case querySpec.OpAnd, querySpec.OpOr, querySpec.OpNot:
		parts := make([]part, len(f.Filters))
		for i := range f.Filters {
			parts[i] = c.compile(s, &f.Filters[i])
		}
		switch f.Op {
		case querySpec.OpAnd:
			return and(parts)
		case querySpec.OpOr:
			return or(parts)
		}
		return not(parts[0])
	}
	return c.condition(s, *f)
}

func (c *compiler) condition(s source, f querySpec.Filter) part {
	switch f.Field {
	case "text":
		return c.words(s, f, s.text(c.reach))
	case "title":
		if len(s.title) == 0 {
			return negatedIf(f.Operator == querySpec.OperatorNotContains, partNever)
		}
		return c.words(s, f, s.title)
	case "type":
		yes := false
		for _, v := range f.Values {
			if v.Text == s.kind {
				yes = true
			}
		}
		if f.Operator == querySpec.OperatorIsNot {
			yes = !yes
		}
		if yes {
			return partAlways
		}
		return partNever
	case "created", "updated":
		return c.date(s, f)
	case "from", fieldAssignee:
		return c.people(s, f)
	case "in":
		return c.places(s, f)
	case fieldStatus, fieldPriority, fieldProject:
		return c.keywords(s, f)
	}
	// A field the index can't filter on yet (an area's own, say has:, is: or
	// due): left out, and said.
	c.unfiltered[f.Field] = true
	return partAlways
}

func negatedIf(neg bool, p part) part {
	if neg {
		return not(p)
	}
	return p
}

// words is a word or a phrase in fields: each word must match somewhere,
// forgiving small typos, and all of a word the index reads as several ("Q4-12"
// is q4 and 12) in the one field; the one being typed also matches as the
// start of a title; a phrase matches as written.
func (c *compiler) words(s source, f querySpec.Filter, fields []string) part {
	if len(f.Values) != 1 || len(fields) == 0 {
		return partNever
	}
	neg := f.Operator == querySpec.OperatorNotContains
	if f.Operator != querySpec.OperatorContains && !neg {
		c.unfiltered[f.Field] = true
		return partAlways
	}
	text := strings.TrimSpace(f.Values[0].Text)
	if text == "" {
		return partAlways
	}
	if !neg && !c.allStop && f.Field == "text" && stopWord(text) {
		return partAlways
	}
	var q map[string]any
	if strings.ContainsAny(text, " \t") {
		q = map[string]any{"multi_match": map[string]any{"query": text, "fields": fields, "type": "phrase", "lenient": true}}
	} else {
		mm := map[string]any{"query": text, "fields": fields, "lenient": true, "operator": "and"}
		if fuzzyNames[s.index] || f.Field == "text" {
			mm["fuzziness"] = "AUTO"
			mm["prefix_length"] = 1
		}
		q = map[string]any{"multi_match": mm}
		if !neg && f.Span != nil && f.Span[1] == c.lastWord && len(s.prefix) > 0 {
			should := []map[string]any{q}
			for _, p := range s.prefix {
				should = append(should, map[string]any{"match_phrase_prefix": map[string]any{p: map[string]any{"query": text, "boost": s.prefixBoost}}})
			}
			q = map[string]any{"bool": map[string]any{"should": should, "minimum_should_match": 1}}
		}
	}
	return negatedIf(neg, query(q))
}

// date is a date condition on the source's created or updated field.
func (c *compiler) date(s source, f querySpec.Filter) part {
	field, ok := s.dates[f.Field]
	if ok && len(c.ready(s, f.Field, []string{field})) == 0 {
		return partNever
	}
	if !ok {
		return negatedIf(f.Operator == querySpec.OperatorIsNot || f.Operator == querySpec.OperatorEmpty, partNever)
	}
	switch f.Operator {
	case querySpec.OperatorEmpty:
		return not(filterOnly(map[string]any{"exists": map[string]any{"field": field}}))
	case querySpec.OperatorNotEmpty:
		return filterOnly(map[string]any{"exists": map[string]any{"field": field}})
	}
	window := func(v querySpec.Value) (int64, int64, error) {
		from, to, err := v.Window(c.now, c.zone, f.Operator, false)
		return from.Unix(), to.Unix(), err
	}
	if len(f.Values) == 0 {
		return partAlways
	}
	from, to, err := window(f.Values[0])
	if err != nil {
		c.unfiltered[f.Field] = true
		return partAlways
	}
	rng := map[string]any{}
	switch f.Operator {
	case querySpec.OperatorIs, querySpec.OperatorWithin:
		rng["gte"], rng["lt"] = from, to
	case querySpec.OperatorIsNot:
		return not(filterOnly(map[string]any{"range": map[string]any{field: map[string]any{"gte": from, "lt": to}}}))
	case querySpec.OperatorBefore:
		rng["lt"] = from
	case querySpec.OperatorOnOrBefore:
		rng["lt"] = to
	case querySpec.OperatorAfter:
		rng["gte"] = to
	case querySpec.OperatorOnOrAfter:
		rng["gte"] = from
	case querySpec.OperatorBetween:
		if len(f.Values) != 2 {
			return partNever
		}
		_, end, err := window(f.Values[1])
		if err != nil {
			c.unfiltered[f.Field] = true
			return partAlways
		}
		rng["gte"], rng["lt"] = from, end
	default:
		c.unfiltered[f.Field] = true
		return partAlways
	}
	return filterOnly(map[string]any{"range": map[string]any{field: rng}})
}

// people is from: or assignee: as ids, with me and my team read for the
// asker.
func (c *compiler) people(s source, f querySpec.Filter) part {
	mapped := s.fields[f.Field]
	fields := c.ready(s, f.Field, mapped)
	neg := f.Operator == querySpec.OperatorNotIn || f.Operator == querySpec.OperatorIsNot
	if len(mapped) > 0 && len(fields) == 0 {
		return partNever
	}
	if len(fields) == 0 {
		return negatedIf(neg, partNever)
	}
	switch f.Operator {
	case querySpec.OperatorEmpty, querySpec.OperatorNotEmpty:
		exists := filterOnly(anyOf(existsAll(fields)...))
		return negatedIf(f.Operator == querySpec.OperatorEmpty, exists)
	case querySpec.OperatorIn, querySpec.OperatorNotIn, querySpec.OperatorIs, querySpec.OperatorIsNot, querySpec.OperatorAllOf:
	default:
		c.unfiltered[f.Field] = true
		return partAlways
	}
	var ids []string
	for _, v := range f.Values {
		switch {
		case v.Text == querySpec.Me:
			ids = append(ids, c.reach.UserUUID)
		case v.Text == querySpec.MyTeam:
			ids = append(ids, c.teamMates()...)
		case v.ID != "":
			ids = append(ids, v.ID)
		}
	}
	if len(ids) == 0 {
		return negatedIf(neg, partNever)
	}
	var qs []map[string]any
	for _, field := range fields {
		qs = append(qs, terms(field, ids))
	}
	return negatedIf(neg, filterOnly(anyOf(qs...)))
}

func existsAll(fields []string) []map[string]any {
	out := make([]map[string]any, len(fields))
	for i, f := range fields {
		out[i] = map[string]any{"exists": map[string]any{"field": f}}
	}
	return out
}

func (c *compiler) teamMates() []string {
	if !c.matesSet {
		c.matesSet = true
		c.mates, c.matesErr = teamMates(c.ctx, "", c.reach.Teams)
		if c.matesErr != nil {
			helpers.LogErrorWithContext(c.ctx, "search: could not read the asker's team mates for @my_team: %v", c.matesErr)
		}
	}
	return c.mates
}

// places is in: as the fields that hold each place here: a channel, the
// asker's own DM with a person, a project or a team.
func (c *compiler) places(s source, f querySpec.Filter) part {
	neg := f.Operator == querySpec.OperatorIsNot
	var qs []map[string]any
	for _, v := range f.Values {
		var fields []string
		id := v.ID
		switch v.Type {
		case "channel":
			fields = s.fields[inChannel]
		case searchQuery.TypeDMWith:
			fields = s.fields[inDM]
			id = helpers.GetGroupingId(c.reach.UserUUID, v.ID)
		case placeConversation:
			fields = s.fields[inDM]
		case "project":
			fields = s.fields[inProject]
		case "team":
			fields = s.fields[inTeam]
		}
		for _, field := range fields {
			qs = append(qs, term(field, id))
		}
	}
	if len(qs) == 0 {
		return negatedIf(neg, partNever)
	}
	return negatedIf(neg, filterOnly(anyOf(qs...)))
}

// keywords are a task field's values as the index holds them: a status or a
// priority by name, a project by id.
func (c *compiler) keywords(s source, f querySpec.Filter) part {
	fields := s.fields[f.Field]
	neg := f.Operator == querySpec.OperatorIsNot || f.Operator == querySpec.OperatorNotIn
	if len(fields) == 0 {
		return negatedIf(neg, partNever)
	}
	switch f.Operator {
	case querySpec.OperatorIs, querySpec.OperatorIsNot, querySpec.OperatorIn, querySpec.OperatorNotIn:
	default:
		c.unfiltered[f.Field] = true
		return partAlways
	}
	var values []string
	for _, v := range f.Values {
		switch {
		case v.ID != "":
			values = append(values, v.ID)
		case v.Text != "":
			values = append(values, v.Text)
		}
	}
	var qs []map[string]any
	for _, field := range fields {
		qs = append(qs, terms(field, values))
	}
	return negatedIf(neg, filterOnly(anyOf(qs...)))
}

// unfilteredNotes say which fields search couldn't filter on.
func (c *compiler) unfilteredNotes() []searchQuery.Issue {
	names := make([]string, 0, len(c.unfiltered))
	for name := range c.unfiltered {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []searchQuery.Issue
	partial := make([]string, 0, len(c.partial))
	for name := range c.partial {
		partial = append(partial, name)
	}
	sort.Strings(partial)
	for _, name := range partial {
		out = append(out, searchQuery.Issue{Code: "partial", Level: searchQuery.LevelNote,
			Message: fmt.Sprintf("Some entries on this server can't be filtered by %s: yet, so they were left out.", name)})
	}
	for _, name := range names {
		out = append(out, searchQuery.Issue{Code: "unfiltered", Level: searchQuery.LevelNote,
			Message: fmt.Sprintf("Search can't filter by %s: yet, so that part was left out.", name)})
	}
	return out
}
